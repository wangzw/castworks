package connectorhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testJobPayload = `{"job":{"id":"j-1","project":"demo","status":"completed",` +
	`"conclusion":"succeeded","updated_at":"2026-11-20T00:59:00Z"}}`

func testNow() time.Time {
	return time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
}

// testSHA256 是测试里独立实现的 SHA-256 十六进制，用来核对派生值。
func testSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func insertTestEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ev connectorEvent) {
	t.Helper()
	correlation := ev.Correlation
	if correlation == nil {
		correlation = map[string]string{}
	}
	encoded, err := json.Marshal(correlation)
	if err != nil {
		t.Fatalf("encode event correlation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO connector_event
		(append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`,
		ev.AppendSeq, ev.System, ev.DeliveryID, ev.EventType, ev.ProjectIDs,
		string(encoded), ev.OccurredAt, ev.Payload); err != nil {
		t.Fatalf("insert event %s: %v", ev.DeliveryID, err)
	}
}

func insertTestSubscription(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sub subscription) {
	t.Helper()
	correlation := sub.Correlation
	if correlation == nil {
		correlation = map[string]string{}
	}
	encodedCorrelation, err := json.Marshal(correlation)
	if err != nil {
		t.Fatalf("encode subscription correlation: %v", err)
	}
	dedupSpec, err := json.Marshal(sub.DedupSpec)
	if err != nil {
		t.Fatalf("encode subscription dedup spec: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscription
		(sub_id, run_id, project_id, system, event, correlation, status, terminal,
		 since_cursor, dedup_spec, created_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10::jsonb, $11, $12)`,
		sub.SubID, sub.RunID, sub.ProjectID, sub.System, sub.Event,
		string(encodedCorrelation), sub.Status, sub.Terminal, sub.SinceCursor,
		string(dedupSpec), sub.CreatedAt, sub.CompletedAt); err != nil {
		t.Fatalf("insert subscription %s: %v", sub.SubID, err)
	}
}

func readTestSubscription(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subID string) (status, terminal string, completedAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT status, terminal, completed_at FROM subscription WHERE sub_id = $1`, subID,
	).Scan(&status, &terminal, &completedAt); err != nil {
		t.Fatalf("read subscription %s: %v", subID, err)
	}
	return status, terminal, completedAt
}

func countTestRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, subID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+table+` WHERE sub_id = $1`, subID).Scan(&count); err != nil {
		t.Fatalf("count %s for %s: %v", table, subID, err)
	}
	return count
}

func loadTestCompletions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subID string) []Completion {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT event FROM completion_delivery WHERE sub_id = $1 ORDER BY created_at, dedup_key`, subID)
	if err != nil {
		t.Fatalf("query completions for %s: %v", subID, err)
	}
	defer rows.Close()

	var completions []Completion
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan completion for %s: %v", subID, err)
		}
		var completion Completion
		if err := json.Unmarshal(raw, &completion); err != nil {
			t.Fatalf("decode completion for %s: %v", subID, err)
		}
		completions = append(completions, completion)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read completions for %s: %v", subID, err)
	}
	return completions
}

func jobConclusion(payload []byte) (string, error) {
	var body struct {
		Job struct {
			Conclusion string `json:"conclusion"`
		} `json:"job"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", err
	}
	if body.Job.Conclusion == "" {
		return "", fmt.Errorf("payload has no job.conclusion")
	}
	return body.Job.Conclusion, nil
}

func defaultTestSubscription(subID string) subscription {
	return subscription{
		SubID:       subID,
		RunID:       "run-1",
		ProjectID:   "demo",
		System:      "extsim",
		Event:       "job.completed",
		Correlation: map[string]string{"job.id": "j-1"},
		Status:      "active",
		SinceCursor: 2,
		DedupSpec:   []string{"event_type", "job.id", "job.conclusion"},
		CreatedAt:   testNow().Add(-time.Hour),
	}
}

func defaultTestEvent(seq int64, deliveryID string, occurredAt time.Time) connectorEvent {
	return connectorEvent{
		AppendSeq:   seq,
		System:      "extsim",
		DeliveryID:  deliveryID,
		EventType:   "job.completed",
		ProjectIDs:  []string{"demo"},
		Correlation: map[string]string{"job.id": "j-1", "job.project": "demo"},
		OccurredAt:  occurredAt,
		Payload:     []byte(testJobPayload),
	}
}

func TestMatchEventCompletesSubscription(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	occurredAt := now.Add(-time.Minute)

	ev := defaultTestEvent(1, "d-1", occurredAt)
	insertTestEvent(t, ctx, pool, ev)
	insertTestSubscription(t, ctx, pool, defaultTestSubscription("sub-1"))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	completed, err := matchEvent(ctx, tx, ev, jobConclusionOrFail(t, ev.Payload), now)
	if err != nil {
		t.Fatalf("matchEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if completed != 1 {
		t.Fatalf("matchEvent completed %d subscriptions, want 1", completed)
	}

	status, terminal, completedAt := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed and succeeded", status, terminal)
	}
	if completedAt == nil || !completedAt.Equal(now) {
		t.Fatalf("completed_at = %v, want %v", completedAt, now)
	}
	if got := countTestRows(t, ctx, pool, "subscription_claim", "sub-1"); got != 1 {
		t.Fatalf("subscription_claim rows = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 1 {
		t.Fatalf("completion_delivery rows = %d, want 1", got)
	}

	completions := loadTestCompletions(t, ctx, pool, "sub-1")
	completion := completions[0]
	wantDedup := testSHA256(`["sub-1","` + testSHA256(`["job.completed","j-1","succeeded"]`) + `"]`)
	if completion.EventID != `["sub-1","succeeded"]` {
		t.Errorf("event_id = %q, want %q", completion.EventID, `["sub-1","succeeded"]`)
	}
	if completion.DedupKey != wantDedup {
		t.Errorf("dedup_key = %q, want %q", completion.DedupKey, wantDedup)
	}
	if completion.SubID != "sub-1" || completion.System != "extsim" || completion.Event != "job.completed" {
		t.Errorf("completion identity = %q/%q/%q", completion.SubID, completion.System, completion.Event)
	}
	if completion.SubscriptionStatus != "completed" || completion.Terminal != "succeeded" {
		t.Errorf("completion status = %q terminal = %q", completion.SubscriptionStatus, completion.Terminal)
	}
	if completion.Source != "webhook" {
		t.Errorf("completion source = %q, want webhook", completion.Source)
	}
	if !completion.OccurredAt.Equal(occurredAt) {
		t.Errorf("completion occurred_at = %v, want %v", completion.OccurredAt, occurredAt)
	}
	if completion.Detail != testJobPayload || completion.DetailTruncated {
		t.Errorf("completion detail = %d bytes truncated=%v, want raw payload untruncated",
			len(completion.Detail), completion.DetailTruncated)
	}
	if got := completion.Correlation["job.id"]; got != "j-1" {
		t.Errorf("completion correlation job.id = %q, want j-1", got)
	}
	if len(completion.Correlation) != 1 {
		t.Errorf("completion correlation = %v, want only the subscription correlation", completion.Correlation)
	}
}

func jobConclusionOrFail(t *testing.T, payload []byte) string {
	t.Helper()
	terminal, err := jobConclusion(payload)
	if err != nil {
		t.Fatalf("extract terminal: %v", err)
	}
	return terminal
}

func TestMatchEventIdempotent(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	ev := defaultTestEvent(1, "d-1", now.Add(-time.Minute))
	insertTestEvent(t, ctx, pool, ev)
	insertTestSubscription(t, ctx, pool, defaultTestSubscription("sub-1"))

	match := func(ev connectorEvent) int {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		n, err := matchEvent(ctx, tx, ev, "succeeded", now)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return n
	}

	if n := match(ev); n != 1 {
		t.Fatalf("first match completed %d, want 1", n)
	}
	if n := match(ev); n != 0 {
		t.Fatalf("repeated match completed %d, want 0", n)
	}

	// 投递标识不同、内容相同的事件在订阅完成后到达，同样不产生第二个完成事件。
	duplicate := ev
	duplicate.DeliveryID = "d-2"
	duplicate.AppendSeq = 2
	insertTestEvent(t, ctx, pool, duplicate)
	if n := match(duplicate); n != 0 {
		t.Fatalf("content duplicate completed %d, want 0", n)
	}

	if got := countTestRows(t, ctx, pool, "subscription_claim", "sub-1"); got != 1 {
		t.Errorf("subscription_claim rows = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 1 {
		t.Errorf("completion_delivery rows = %d, want 1", got)
	}
}

func TestMatchEventNonMatches(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	ev := defaultTestEvent(1, "d-1", now.Add(-time.Minute))
	insertTestEvent(t, ctx, pool, ev)

	subs := map[string]subscription{
		"sub-match": defaultTestSubscription("sub-match"),
		"sub-project": func() subscription {
			sub := defaultTestSubscription("sub-project")
			sub.ProjectID = "other"
			return sub
		}(),
		"sub-event": func() subscription {
			sub := defaultTestSubscription("sub-event")
			sub.Event = "job.failed"
			return sub
		}(),
	}
	extraCorrelation := defaultTestSubscription("sub-correlation")
	extraCorrelation.Correlation = map[string]string{"job.id": "j-1", "job.owner": "acme"}
	subs[extraCorrelation.SubID] = extraCorrelation
	for _, sub := range subs {
		insertTestSubscription(t, ctx, pool, sub)
	}

	// project_ids 为空的事件不参与匹配。
	unbound := defaultTestEvent(2, "d-2", now.Add(-time.Minute))
	unbound.ProjectIDs = []string{}
	insertTestEvent(t, ctx, pool, unbound)

	match := func(ev connectorEvent) int {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		n, err := matchEvent(ctx, tx, ev, "succeeded", now)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return n
	}

	if n := match(unbound); n != 0 {
		t.Fatalf("unbound event completed %d subscriptions, want 0", n)
	}
	if n := match(ev); n != 1 {
		t.Fatalf("event completed %d subscriptions, want only sub-match", n)
	}

	for subID := range subs {
		status, _, _ := readTestSubscription(t, ctx, pool, subID)
		want := "active"
		if subID == "sub-match" {
			want = "completed"
		}
		if status != want {
			t.Errorf("subscription %s status = %q, want %q", subID, status, want)
		}
	}
}

func TestMatchEventCASFailureWritesNothing(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	ev := defaultTestEvent(1, "d-1", now.Add(-time.Minute))
	insertTestEvent(t, ctx, pool, ev)

	for _, status := range []string{"completed", "cancelled"} {
		concludedAt := now.Add(-time.Hour)
		sub := defaultTestSubscription("sub-" + status)
		sub.Status = status
		sub.CompletedAt = &concludedAt
		insertTestSubscription(t, ctx, pool, sub)

		// 调用方手里的订阅行仍是 active，模拟候选读取与条件更新之间的并发完成。
		stale := sub
		stale.Status = "active"

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ok, err := completeWithEvent(ctx, tx, stale, ev, "succeeded", now)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("completeWithEvent for %s subscription: %v", status, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if ok {
			t.Errorf("%s subscription was completed again", status)
		}
		if got := countTestRows(t, ctx, pool, "subscription_claim", sub.SubID); got != 0 {
			t.Errorf("%s subscription has %d claims, want 0", status, got)
		}
		if got := countTestRows(t, ctx, pool, "completion_delivery", sub.SubID); got != 0 {
			t.Errorf("%s subscription has %d deliveries, want 0", status, got)
		}
	}

	// matchEvent 的候选查询只取 active，已完成或已取消的订阅根本不会进入匹配。
	for _, status := range []string{"completed", "cancelled"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		n, err := matchEvent(ctx, tx, ev, "succeeded", now)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if n != 0 {
			t.Errorf("matchEvent completed %d subscriptions for %s subscriber, want 0", n, status)
		}
	}
}

func TestMatchSubscriptionBacktrackAndRescan(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	base := now.Add(-time.Hour)

	events := []connectorEvent{
		defaultTestEvent(1, "d-1", base.Add(1*time.Minute)),
		defaultTestEvent(2, "d-2", base.Add(2*time.Minute)),
		defaultTestEvent(3, "d-3", base.Add(3*time.Minute)),
	}
	events[0].Correlation = map[string]string{"job.id": "j-2", "job.project": "demo"}
	events[0].Payload = []byte(`{"job":{"id":"j-2","project":"demo","conclusion":"failed"}}`)
	for _, ev := range events {
		insertTestEvent(t, ctx, pool, ev)
	}

	match := func(sub subscription, from, to int64) bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ok, err := matchSubscription(ctx, tx, sub, from, to, now, jobConclusion)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchSubscription: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return ok
	}

	t.Run("backtrack before since_cursor", func(t *testing.T) {
		sub := defaultTestSubscription("sub-backtrack")
		sub.SinceCursor = 3
		insertTestSubscription(t, ctx, pool, sub)

		if ok := match(sub, 1, sub.SinceCursor); !ok {
			t.Fatal("backtrack did not complete the subscription")
		}
		status, terminal, _ := readTestSubscription(t, ctx, pool, sub.SubID)
		if status != "completed" || terminal != "succeeded" {
			t.Fatalf("subscription = %q/%q, want completed/succeeded", status, terminal)
		}
		completions := loadTestCompletions(t, ctx, pool, sub.SubID)
		if len(completions) != 1 || !completions[0].OccurredAt.Equal(events[1].OccurredAt) {
			t.Fatalf("completion occurred_at = %v, want the matching event at %v",
				completions, events[1].OccurredAt)
		}
	})

	t.Run("rescan from since_cursor", func(t *testing.T) {
		sub := defaultTestSubscription("sub-rescan")
		sub.SinceCursor = 3
		insertTestSubscription(t, ctx, pool, sub)

		if ok := match(sub, sub.SinceCursor, math.MaxInt64); !ok {
			t.Fatal("rescan did not complete the subscription")
		}
		completions := loadTestCompletions(t, ctx, pool, sub.SubID)
		if len(completions) != 1 || !completions[0].OccurredAt.Equal(events[2].OccurredAt) {
			t.Fatalf("completion occurred_at = %v, want the event at %v", completions, events[2].OccurredAt)
		}
	})

	t.Run("earliest occurred_at wins", func(t *testing.T) {
		sub := defaultTestSubscription("sub-earliest")
		insertTestSubscription(t, ctx, pool, sub)

		late := defaultTestEvent(4, "d-4", base.Add(5*time.Minute))
		early := defaultTestEvent(5, "d-5", base.Add(4*time.Minute))
		insertTestEvent(t, ctx, pool, late)
		insertTestEvent(t, ctx, pool, early)

		if ok := match(sub, 4, 6); !ok {
			t.Fatal("match did not complete the subscription")
		}
		completions := loadTestCompletions(t, ctx, pool, sub.SubID)
		if len(completions) != 1 || !completions[0].OccurredAt.Equal(early.OccurredAt) {
			t.Fatalf("completion occurred_at = %v, want earliest %v", completions, early.OccurredAt)
		}
		var claimed string
		if err := pool.QueryRow(ctx,
			`SELECT delivery_id FROM subscription_claim WHERE sub_id = $1`, sub.SubID).Scan(&claimed); err != nil {
			t.Fatalf("read claim: %v", err)
		}
		if claimed != early.DeliveryID {
			t.Fatalf("claimed delivery = %q, want %q", claimed, early.DeliveryID)
		}
	})

	t.Run("no match writes nothing", func(t *testing.T) {
		sub := defaultTestSubscription("sub-none")
		insertTestSubscription(t, ctx, pool, sub)

		if ok := match(sub, 10, 20); ok {
			t.Fatal("match completed a subscription without a matching event")
		}
		status, _, _ := readTestSubscription(t, ctx, pool, sub.SubID)
		if status != "active" {
			t.Fatalf("subscription status = %q, want active", status)
		}
		if got := countTestRows(t, ctx, pool, "subscription_claim", sub.SubID); got != 0 {
			t.Errorf("claims = %d, want 0", got)
		}
		if got := countTestRows(t, ctx, pool, "completion_delivery", sub.SubID); got != 0 {
			t.Errorf("deliveries = %d, want 0", got)
		}
	})

	t.Run("inactive subscription is ignored", func(t *testing.T) {
		concludedAt := now.Add(-time.Minute)
		sub := defaultTestSubscription("sub-inactive")
		sub.Status = "cancelled"
		sub.CompletedAt = &concludedAt
		insertTestSubscription(t, ctx, pool, sub)

		if ok := match(sub, 1, math.MaxInt64); ok {
			t.Fatal("cancelled subscription was matched")
		}
		if got := countTestRows(t, ctx, pool, "completion_delivery", sub.SubID); got != 0 {
			t.Errorf("deliveries = %d, want 0", got)
		}
	})
}

func TestMatchSubscriptionIdempotent(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	ev := defaultTestEvent(1, "d-1", now.Add(-time.Minute))
	insertTestEvent(t, ctx, pool, ev)
	sub := defaultTestSubscription("sub-1")
	insertTestSubscription(t, ctx, pool, sub)

	match := func() bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ok, err := matchSubscription(ctx, tx, sub, 1, math.MaxInt64, now, jobConclusion)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchSubscription: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return ok
	}

	if !match() {
		t.Fatal("first scan did not complete the subscription")
	}
	if match() {
		t.Fatal("second scan completed the subscription again")
	}
	if got := countTestRows(t, ctx, pool, "subscription_claim", "sub-1"); got != 1 {
		t.Errorf("claims = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 1 {
		t.Errorf("deliveries = %d, want 1", got)
	}
}
