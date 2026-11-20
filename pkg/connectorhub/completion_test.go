package connectorhub

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

func readTestDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subID string) (status string, attempts int, lastError string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts, last_error FROM completion_delivery WHERE sub_id = $1`, subID,
	).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("read delivery for %s: %v", subID, err)
	}
	return status, attempts, lastError
}

func TestCompletionFromStatus(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	detail := []byte(`{"status":"completed","conclusion":"succeeded"}`)

	sub := defaultTestSubscription("sub-1")
	insertTestSubscription(t, ctx, pool, sub)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ok, err := completeFromStatus(ctx, tx, sub, "succeeded", detail, now)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("completeFromStatus: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !ok {
		t.Fatal("completeFromStatus did not complete the subscription")
	}

	status, terminal, completedAt := readTestSubscription(t, ctx, pool, sub.SubID)
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = %q/%q, want completed/succeeded", status, terminal)
	}
	if completedAt == nil || !completedAt.Equal(now) {
		t.Fatalf("completed_at = %v, want %v", completedAt, now)
	}
	deliveryStatus, attempts, lastError := readTestDelivery(t, ctx, pool, sub.SubID)
	if deliveryStatus != "pending" || attempts != 0 || lastError != "" {
		t.Fatalf("delivery = %q attempts %d last_error %q, want pending/0/empty",
			deliveryStatus, attempts, lastError)
	}

	completions := loadTestCompletions(t, ctx, pool, sub.SubID)
	if len(completions) != 1 {
		t.Fatalf("completions = %d, want 1", len(completions))
	}
	completion := completions[0]
	if completion.EventID != `["sub-1","succeeded"]` {
		t.Errorf("event_id = %q", completion.EventID)
	}
	wantDedup := testSHA256(`["sub-1","verify","succeeded"]`)
	if completion.DedupKey != wantDedup {
		t.Errorf("dedup_key = %q, want %q", completion.DedupKey, wantDedup)
	}
	if completion.Source != "verify" {
		t.Errorf("source = %q, want verify", completion.Source)
	}
	if !completion.OccurredAt.Equal(now) {
		t.Errorf("occurred_at = %v, want the query time %v", completion.OccurredAt, now)
	}
	if completion.Detail != string(detail) || completion.DetailTruncated {
		t.Errorf("detail = %q truncated=%v, want the status response body untruncated",
			completion.Detail, completion.DetailTruncated)
	}

	// 再次以状态查询完成同一订阅不产生第二条记录。
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	ok, err = completeFromStatus(ctx, tx, sub, "succeeded", detail, now.Add(time.Minute))
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("second completeFromStatus: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit second: %v", err)
	}
	if ok {
		t.Fatal("second completeFromStatus completed the subscription again")
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", sub.SubID); got != 1 {
		t.Errorf("deliveries = %d, want 1", got)
	}
}

func TestCompletionFromStatusCASFailureWritesNothing(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()

	for _, status := range []string{"completed", "cancelled"} {
		concludedAt := now.Add(-time.Hour)
		sub := defaultTestSubscription("sub-" + status)
		sub.Status = status
		sub.CompletedAt = &concludedAt
		if status == "completed" {
			sub.Terminal = "failed"
		}
		insertTestSubscription(t, ctx, pool, sub)

		stale := sub
		stale.Status = "active"

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ok, err := completeFromStatus(ctx, tx, stale, "succeeded", []byte(`{}`), now)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("completeFromStatus for %s subscription: %v", status, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if ok {
			t.Errorf("%s subscription was completed again", status)
		}
		if got := countTestRows(t, ctx, pool, "completion_delivery", sub.SubID); got != 0 {
			t.Errorf("%s subscription has %d deliveries, want 0", status, got)
		}
		gotStatus, gotTerminal, _ := readTestSubscription(t, ctx, pool, sub.SubID)
		if gotStatus != status {
			t.Errorf("subscription status = %q, want unchanged %q", gotStatus, status)
		}
		if status == "completed" && gotTerminal != "failed" {
			t.Errorf("subscription terminal = %q, want unchanged failed", gotTerminal)
		}
	}
}

func paddedPayload(t *testing.T, total int) []byte {
	t.Helper()
	base := []byte(testJobPayload)
	if total < len(base) {
		t.Fatalf("target length %d is shorter than the base payload %d", total, len(base))
	}
	payload := make([]byte, 0, total)
	payload = append(payload, base...)
	payload = append(payload, bytes.Repeat([]byte(" "), total-len(base))...)
	return payload
}

func TestCompletionDetailTruncation(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()

	insertEventAndSub := func(t *testing.T, seq int64, deliveryID, subID string, payload []byte, occurredAt time.Time) connectorEvent {
		t.Helper()
		ev := defaultTestEvent(seq, deliveryID, occurredAt)
		ev.Payload = payload
		insertTestEvent(t, ctx, pool, ev)
		sub := defaultTestSubscription(subID)
		insertTestSubscription(t, ctx, pool, sub)
		return ev
	}

	t.Run("webhook at the limit", func(t *testing.T) {
		payload := paddedPayload(t, completionDetailLimit)
		if len(payload) != completionDetailLimit {
			t.Fatalf("payload length = %d", len(payload))
		}
		ev := insertEventAndSub(t, 1, "d-1", "sub-exact", payload, now.Add(-time.Minute))

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := matchEvent(ctx, tx, ev, "succeeded", now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		completions := loadTestCompletions(t, ctx, pool, "sub-exact")
		if len(completions) != 1 {
			t.Fatalf("completions = %d, want 1", len(completions))
		}
		if got := len(completions[0].Detail); got != completionDetailLimit {
			t.Errorf("detail length = %d, want %d", got, completionDetailLimit)
		}
		if completions[0].DetailTruncated {
			t.Error("detail at the limit was marked truncated")
		}
		if completions[0].Detail != string(payload) {
			t.Error("detail at the limit does not equal the raw payload")
		}
	})

	t.Run("webhook over the limit", func(t *testing.T) {
		payload := paddedPayload(t, completionDetailLimit+1)
		ev := insertEventAndSub(t, 2, "d-2", "sub-over", payload, now.Add(-time.Minute))

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := matchEvent(ctx, tx, ev, "succeeded", now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		completions := loadTestCompletions(t, ctx, pool, "sub-over")
		if len(completions) != 1 {
			t.Fatalf("completions = %d, want 1", len(completions))
		}
		if got := len(completions[0].Detail); got != completionDetailLimit {
			t.Errorf("detail length = %d, want %d", got, completionDetailLimit)
		}
		if !completions[0].DetailTruncated {
			t.Error("detail over the limit was not marked truncated")
		}
		if completions[0].Detail != string(payload[:completionDetailLimit]) {
			t.Error("detail is not the first 48 KiB of the raw payload")
		}
	})

	t.Run("verify at and over the limit", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			length int
			subID  string
		}{
			{"at the limit", completionDetailLimit, "sub-verify-exact"},
			{"over the limit", completionDetailLimit + 1, "sub-verify-over"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := bytes.Repeat([]byte("s"), tc.length)
				sub := defaultTestSubscription(tc.subID)
				insertTestSubscription(t, ctx, pool, sub)

				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				ok, err := completeFromStatus(ctx, tx, sub, "succeeded", body, now)
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("completeFromStatus: %v", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit: %v", err)
				}
				if !ok {
					t.Fatal("subscription was not completed")
				}

				completions := loadTestCompletions(t, ctx, pool, tc.subID)
				if len(completions) != 1 {
					t.Fatalf("completions = %d, want 1", len(completions))
				}
				wantTruncated := tc.length > completionDetailLimit
				if got := len(completions[0].Detail); got != completionDetailLimit && wantTruncated {
					t.Errorf("detail length = %d, want %d", got, completionDetailLimit)
				}
				if completions[0].DetailTruncated != wantTruncated {
					t.Errorf("detail_truncated = %v, want %v", completions[0].DetailTruncated, wantTruncated)
				}
				if !wantTruncated && completions[0].Detail != string(body) {
					t.Error("detail at the limit does not equal the response body")
				}
			})
		}
	})
}

// TestCompletionDetailTruncationKeepsUTF8Boundary 让一个多字节字符正好跨在
// 截断边界上，断言截断结果仍是合法 UTF-8、长度不超过上限，且经 JSON 往返
// 长度不变。
func TestCompletionDetailTruncationKeepsUTF8Boundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		width int
	}{
		{name: "two-byte rune", value: "é", width: 2},
		{name: "three-byte rune", value: "中", width: 3},
		{name: "four-byte rune", value: "😀", width: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// 前缀让这个字符的最后一个字节正好落在 completionDetailLimit-1，
			// 按字节截断会把字符切开。
			detail := strings.Repeat("a", completionDetailLimit-(tc.width-1)) +
				tc.value + strings.Repeat("b", 16)
			truncated, ok := truncateDetail(detail)
			if !ok {
				t.Fatal("truncateDetail did not report a truncation")
			}
			if !utf8.ValidString(truncated) {
				t.Fatalf("truncated detail is not valid UTF-8, tail = % x", truncated[len(truncated)-8:])
			}
			if len(truncated) > completionDetailLimit {
				t.Fatalf("truncated length = %d, want at most %d", len(truncated), completionDetailLimit)
			}
			if want := completionDetailLimit - (tc.width - 1); len(truncated) != want {
				t.Fatalf("truncated length = %d, want %d", len(truncated), want)
			}

			encoded, err := json.Marshal(struct {
				Detail string `json:"detail"`
			}{Detail: truncated})
			if err != nil {
				t.Fatalf("marshal detail: %v", err)
			}
			var decoded struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal detail: %v", err)
			}
			if decoded.Detail != truncated || len(decoded.Detail) != len(truncated) {
				t.Fatalf("JSON round trip changed the detail: %d bytes -> %d bytes",
					len(truncated), len(decoded.Detail))
			}
		})
	}
}

// protoCheckingRuns 是 Runs 的测试替身。AppendInbox 按投递时必经的 protobuf
// string 编码校验完成事件：非法 UTF-8 的 detail 会让 proto.Marshal 失败，
// 这里同样以 ErrInvalidEvent 拒绝。其余行为与 hubFakeRuns 相同。
type protoCheckingRuns struct {
	*hubFakeRuns
}

func (f *protoCheckingRuns) AppendInbox(ctx context.Context, runID string, ev runcontroller.InboxEvent) (int64, bool, error) {
	if ev.Subscription != nil {
		if _, err := proto.Marshal(&agentprotocolv1.SubscriptionCompleted{
			SubId:  ev.Subscription.SubID,
			Detail: ev.Subscription.Detail,
		}); err != nil {
			return 0, false, runcontroller.ErrInvalidEvent
		}
	}
	return f.hubFakeRuns.AppendInbox(ctx, runID, ev)
}

// TestCompletionDetailNormalizesInvalidUTF8 覆盖请求体或状态查询响应体本身
// 含非法 UTF-8 字节的情况：构造出的 detail 必须是合法 UTF-8，替换后的长度
// 计入 48 KiB 上限，并且能经 AppendInbox 写入收件箱。
func TestCompletionDetailNormalizesInvalidUTF8(t *testing.T) {
	t.Parallel()

	const invalid = "\xff"

	// 非法字节放在不影响关联键、事件时间与终态取值的字段里，请求体仍是
	// 合法 JSON，原始字节原样进入事件负载。
	webhookBody := strings.Replace(testWebhookPayload,
		`"conclusion":"succeeded"`,
		`"conclusion":"succeeded","note":"`+invalid+`"`, 1)

	assertDetail := func(t *testing.T, c Completion) {
		t.Helper()
		if !utf8.ValidString(c.Detail) {
			tail := c.Detail
			if len(tail) > 8 {
				tail = tail[len(tail)-8:]
			}
			t.Errorf("detail is not valid UTF-8, tail = % x", []byte(tail))
		}
		if len(c.Detail) > completionDetailLimit {
			t.Errorf("detail length = %d, want at most %d", len(c.Detail), completionDetailLimit)
		}
		if !strings.Contains(c.Detail, "�") {
			t.Error("detail does not contain the U+FFFD replacement for the invalid bytes")
		}

		runs := &protoCheckingRuns{hubFakeRuns: &hubFakeRuns{}}
		if _, err := (runDelivery{runs}).AppendCompletion(t.Context(), "run-1", c); err != nil {
			t.Fatalf("AppendInbox rejected the completion: %v", err)
		}
		if calls := runs.callsSnapshot(); len(calls) != 1 {
			t.Fatalf("AppendInbox calls = %d, want 1", len(calls))
		}
	}

	t.Run("webhook under the limit", func(t *testing.T) {
		ev := defaultTestEvent(1, "d-utf8-1", testNow().Add(-time.Minute))
		ev.Payload = []byte(webhookBody)

		completion := newWebhookCompletion(defaultTestSubscription("sub-utf8-webhook"), ev, "succeeded")
		if completion.DetailTruncated {
			t.Fatal("detail under the limit was marked truncated")
		}
		assertDetail(t, completion)
	})

	t.Run("webhook over the limit", func(t *testing.T) {
		payload := webhookBody + strings.Repeat(" ", completionDetailLimit-len(webhookBody)+1)
		ev := defaultTestEvent(2, "d-utf8-2", testNow().Add(-time.Minute))
		ev.Payload = []byte(payload)

		completion := newWebhookCompletion(defaultTestSubscription("sub-utf8-webhook-over"), ev, "succeeded")
		if !completion.DetailTruncated {
			t.Fatal("detail over the limit was not marked truncated")
		}
		// 替换后的长度计入上限：规范化把 detail 放大 2 字节后仍然恰好截到
		// 上限。先截断再替换会得到超过上限的结果。
		if got := len(completion.Detail); got != completionDetailLimit {
			t.Errorf("detail length = %d, want %d", got, completionDetailLimit)
		}
		assertDetail(t, completion)
	})

	t.Run("status query", func(t *testing.T) {
		body := []byte(`{"status":"completed","note":"` + invalid + `"}`)

		completion := newVerifyCompletion(
			defaultTestSubscription("sub-utf8-verify"), "succeeded", body, testNow())
		if completion.DetailTruncated {
			t.Fatal("detail under the limit was marked truncated")
		}
		assertDetail(t, completion)
	})
}

func TestCompletionDedupValues(t *testing.T) {
	t.Parallel()

	ev := connectorEvent{
		EventType: "job.completed",
		Payload:   []byte(`{"job":{"id":42,"nested":{"x":true}},"list":[1,2]}`),
	}
	got := dedupValues(ev, []string{"event_type", "job.id", "job.missing", "job.nested.x", "list"})
	want := []string{"job.completed", "42", "", "true", "[1,2]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupValues = %#v, want %#v", got, want)
	}

	empty := dedupValues(connectorEvent{EventType: "job.completed"}, []string{"event_type", "job.id"})
	if !reflect.DeepEqual(empty, []string{"job.completed", ""}) {
		t.Fatalf("dedupValues with an empty payload = %#v", empty)
	}
}
