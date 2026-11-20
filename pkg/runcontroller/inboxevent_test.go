package runcontroller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

func testSubscriptionCompletion() *runcontroller.SubscriptionCompletion {
	return &runcontroller.SubscriptionCompletion{
		SubID:              "sub-1",
		System:             "extsim",
		Event:              "job.completed",
		SubscriptionStatus: "completed",
		Terminal:           "succeeded",
		Correlation:        map[string]string{"job_id": "job-42"},
		OccurredAt:         testEpoch.Add(3 * time.Minute),
		Source:             "webhook",
		Detail:             `{"result":"ok"}`,
		DetailTruncated:    true,
	}
}

func testSubscriptionEvent(dedupKey string) runcontroller.InboxEvent {
	return runcontroller.InboxEvent{
		Kind:         runcontroller.KindSubscriptionCompleted,
		EventID:      "evt-" + dedupKey,
		DedupKey:     dedupKey,
		Subscription: testSubscriptionCompletion(),
	}
}

// maxCorrelationBytes 是接口清单为订阅完成事件 correlation 规定的键与值
// 总字节数上限。
const maxCorrelationBytes = 4 << 10

// subscriptionEventWith 返回一份订阅完成事件，subscription 先经过 mutate。
func subscriptionEventWith(dedupKey string, mutate func(*runcontroller.SubscriptionCompletion)) runcontroller.InboxEvent {
	sc := testSubscriptionCompletion()
	mutate(sc)
	return runcontroller.InboxEvent{
		Kind:         runcontroller.KindSubscriptionCompleted,
		EventID:      "evt-" + dedupKey,
		DedupKey:     dedupKey,
		Subscription: sc,
	}
}

func TestAppendInboxValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	ctx := context.Background()

	tooLongDetail := testSubscriptionCompletion()
	tooLongDetail.Detail = strings.Repeat("d", runcontroller.MaxDetailBytes+1)

	cases := []struct {
		name string
		ev   runcontroller.InboxEvent
	}{
		{"human message with empty text", runcontroller.InboxEvent{Kind: runcontroller.KindHumanMessage}},
		{"human message too long", runcontroller.InboxEvent{
			Kind: runcontroller.KindHumanMessage,
			Text: strings.Repeat("x", runcontroller.MaxTextBytes+1),
		}},
		{"unknown kind", runcontroller.InboxEvent{Kind: "bogus"}},
		{"subscription without event id", runcontroller.InboxEvent{
			Kind:         runcontroller.KindSubscriptionCompleted,
			DedupKey:     "dedup-missing-event",
			Subscription: testSubscriptionCompletion(),
		}},
		{"subscription without dedup key", runcontroller.InboxEvent{
			Kind:         runcontroller.KindSubscriptionCompleted,
			EventID:      "evt-missing-dedup",
			Subscription: testSubscriptionCompletion(),
		}},
		{"subscription without payload", runcontroller.InboxEvent{
			Kind:     runcontroller.KindSubscriptionCompleted,
			EventID:  "evt-missing-payload",
			DedupKey: "dedup-missing-payload",
		}},
		{"subscription detail too long", runcontroller.InboxEvent{
			Kind:         runcontroller.KindSubscriptionCompleted,
			EventID:      "evt-long-detail",
			DedupKey:     "dedup-long-detail",
			Subscription: tooLongDetail,
		}},
		{"subscription without sub id", subscriptionEventWith("missing-sub-id", func(sc *runcontroller.SubscriptionCompletion) {
			sc.SubID = ""
		})},
		{"subscription without system", subscriptionEventWith("missing-system", func(sc *runcontroller.SubscriptionCompletion) {
			sc.System = ""
		})},
		{"subscription without event", subscriptionEventWith("missing-event", func(sc *runcontroller.SubscriptionCompletion) {
			sc.Event = ""
		})},
		{"subscription without subscription status", subscriptionEventWith("missing-status", func(sc *runcontroller.SubscriptionCompletion) {
			sc.SubscriptionStatus = ""
		})},
		{"subscription without terminal", subscriptionEventWith("missing-terminal", func(sc *runcontroller.SubscriptionCompletion) {
			sc.Terminal = ""
		})},
		{"subscription without source", subscriptionEventWith("missing-source", func(sc *runcontroller.SubscriptionCompletion) {
			sc.Source = ""
		})},
		{"subscription with zero occurred_at", subscriptionEventWith("zero-occurred-at", func(sc *runcontroller.SubscriptionCompletion) {
			sc.OccurredAt = time.Time{}
		})},
		{"subscription with out of range occurred_at", subscriptionEventWith("out-of-range-occurred-at", func(sc *runcontroller.SubscriptionCompletion) {
			sc.OccurredAt = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
		})},
		{"subscription with empty correlation key", subscriptionEventWith("empty-correlation-key", func(sc *runcontroller.SubscriptionCompletion) {
			sc.Correlation = map[string]string{"": "job-42"}
		})},
		{"subscription with oversize correlation", subscriptionEventWith("oversize-correlation", func(sc *runcontroller.SubscriptionCompletion) {
			sc.Correlation = map[string]string{"k": strings.Repeat("v", maxCorrelationBytes)}
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, tc.ev)
			if !errors.Is(err, runcontroller.ErrInvalidEvent) {
				t.Fatalf("AppendInbox = (%d, %v, %v), 想要 ErrInvalidEvent", seq, duplicate, err)
			}
			if seq != 0 || duplicate {
				t.Fatalf("校验失败时返回 (%d, %v), 想要 (0, false)", seq, duplicate)
			}
		})
	}

	// A detail of exactly the limit is accepted.
	limit := testSubscriptionCompletion()
	limit.Detail = strings.Repeat("d", runcontroller.MaxDetailBytes)
	ev := runcontroller.InboxEvent{
		Kind:         runcontroller.KindSubscriptionCompleted,
		EventID:      "evt-limit-detail",
		DedupKey:     "dedup-limit-detail",
		Subscription: limit,
	}
	if seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev); err != nil || duplicate || seq != 2 {
		t.Fatalf("Detail 恰好 MaxDetailBytes: seq=%d duplicate=%v err=%v, 想要 (2, false, nil)", seq, duplicate, err)
	}

	// The key and value bytes of correlation may total exactly the limit.
	corrLimit := testSubscriptionCompletion()
	corrLimit.Correlation = map[string]string{"k": strings.Repeat("v", maxCorrelationBytes-1)}
	ev = runcontroller.InboxEvent{
		Kind:         runcontroller.KindSubscriptionCompleted,
		EventID:      "evt-limit-correlation",
		DedupKey:     "dedup-limit-correlation",
		Subscription: corrLimit,
	}
	if seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev); err != nil || duplicate || seq != 3 {
		t.Fatalf("correlation 恰好 4 KiB: seq=%d duplicate=%v err=%v, 想要 (3, false, nil)", seq, duplicate, err)
	}
}

// TestAcceptedSubscriptionEventsDeliverValidFrames 验证写入校验与协议校验
// 一致：AppendInbox 接受的订阅完成事件，经投递路径组出的帧都能通过
// agentprotocol.ValidateControlFrame。
func TestAcceptedSubscriptionEventsDeliverValidFrames(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	ctx := context.Background()

	missingSubID := testSubscriptionCompletion()
	missingSubID.SubID = ""
	missingSource := testSubscriptionCompletion()
	missingSource.Source = ""
	zeroOccurredAt := testSubscriptionCompletion()
	zeroOccurredAt.OccurredAt = time.Time{}
	outOfRangeOccurredAt := testSubscriptionCompletion()
	outOfRangeOccurredAt.OccurredAt = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	emptyCorrelationKey := testSubscriptionCompletion()
	emptyCorrelationKey.Correlation = map[string]string{"": "job-42"}
	oversizeCorrelation := testSubscriptionCompletion()
	oversizeCorrelation.Correlation = map[string]string{"k": strings.Repeat("v", maxCorrelationBytes)}

	events := []runcontroller.InboxEvent{
		testSubscriptionEvent("frame-valid"),
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-missing-sub-id", DedupKey: "frame-missing-sub-id",
			Subscription: missingSubID},
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-missing-source", DedupKey: "frame-missing-source",
			Subscription: missingSource},
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-zero", DedupKey: "frame-zero",
			Subscription: zeroOccurredAt},
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-range", DedupKey: "frame-range",
			Subscription: outOfRangeOccurredAt},
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-empty-key", DedupKey: "frame-empty-key",
			Subscription: emptyCorrelationKey},
		{Kind: runcontroller.KindSubscriptionCompleted, EventID: "evt-frame-oversize", DedupKey: "frame-oversize",
			Subscription: oversizeCorrelation},
	}
	for _, ev := range events {
		seq, _, err := h.ctrl.AppendInbox(ctx, runID, ev)
		if err != nil {
			// 不接受的事件不在性质范围内。
			continue
		}
		frame, err := runcontroller.DeliverFrameForTest(h.ctrl, runID, seq, seq)
		if err != nil {
			t.Fatalf("AppendInbox 接受了 %s，但投递帧组不出来: %v", ev.EventID, err)
		}
		if err := agentprotocol.ValidateControlFrame(frame); err != nil {
			t.Fatalf("AppendInbox 接受了 %s，但帧未通过协议校验: %v", ev.EventID, err)
		}
	}
}

func TestAppendInboxDedup(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	ctx := context.Background()

	ev := testSubscriptionEvent("dedup-1")
	seq1, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
	if err != nil {
		t.Fatalf("首次 AppendInbox: %v", err)
	}
	if duplicate {
		t.Fatal("首次 AppendInbox 返回 duplicate = true")
	}
	if seq1 != 2 {
		t.Fatalf("首次 inbox_seq = %d, 想要 2", seq1)
	}

	nextSeq := nextInboxSeq(t, h.pool, runID)
	if nextSeq != seq1+1 {
		t.Fatalf("首次写入后 next_inbox_seq = %d, 想要 %d", nextSeq, seq1+1)
	}

	var (
		kind     string
		eventID  string
		dedupKey string
		payload  []byte
	)
	err = h.pool.QueryRow(ctx,
		`SELECT kind, event_id, dedup_key, payload FROM inbox WHERE run_id = $1 AND inbox_seq = $2`,
		runID, seq1).Scan(&kind, &eventID, &dedupKey, &payload)
	if err != nil {
		t.Fatalf("读取收件箱行: %v", err)
	}
	if kind != string(runcontroller.KindSubscriptionCompleted) || eventID != ev.EventID || dedupKey != ev.DedupKey {
		t.Fatalf("收件箱行 = kind %q event_id %q dedup_key %q", kind, eventID, dedupKey)
	}

	// The payload is the SubscriptionCompletion encoded with Go field names.
	var decoded runcontroller.SubscriptionCompletion
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if !decoded.OccurredAt.Equal(ev.Subscription.OccurredAt) {
		t.Fatalf("OccurredAt = %v, 想要 %v", decoded.OccurredAt, ev.Subscription.OccurredAt)
	}
	if decoded.SubID != ev.Subscription.SubID || decoded.Detail != ev.Subscription.Detail ||
		decoded.DetailTruncated != ev.Subscription.DetailTruncated ||
		decoded.Correlation["job_id"] != "job-42" {
		t.Fatalf("payload 往返不一致: %+v", decoded)
	}
	if !decoded.DetailTruncated {
		t.Fatal("payload 往返后 DetailTruncated = false, 想要 true")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("payload 不是 JSON 对象: %v", err)
	}
	for _, name := range []string{"SubID", "System", "Event", "OccurredAt", "DetailTruncated"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("payload 缺少 Go 字段名 %q: %s", name, payload)
		}
	}

	// The same dedup key returns the existing row and changes nothing.
	seq2, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
	if err != nil {
		t.Fatalf("重复 AppendInbox: %v", err)
	}
	if !duplicate {
		t.Fatal("重复 AppendInbox 的 duplicate = false")
	}
	if seq2 != seq1 {
		t.Fatalf("重复 inbox_seq = %d, 想要 %d", seq2, seq1)
	}
	if rows := countInboxDedup(t, h.pool, runID, ev.DedupKey); rows != 1 {
		t.Fatalf("dedup_key 相同的行数 = %d, 想要 1", rows)
	}
	if got := nextInboxSeq(t, h.pool, runID); got != nextSeq {
		t.Fatalf("重复写入后 next_inbox_seq = %d, 想要 %d", got, nextSeq)
	}
}

func TestAppendInboxDuplicateWhileSuspendedDoesNotResume(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	ctx := context.Background()

	// First suspension: the first write exercises a real wake-up.
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

	ev := testSubscriptionEvent("dedup-suspended")
	seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
	if err != nil {
		t.Fatalf("首次 AppendInbox: %v", err)
	}
	if duplicate {
		t.Fatal("首次 AppendInbox 返回 duplicate = true")
	}
	h.waitCalls(fakeprovider.OpRestore, 1)
	h.waitRestoreReturned(runID)
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)

	// The agent reconnects and confirms it already holds the new message, so
	// the run returns to active/boundary without another delivery. Hello is
	// valid only as the first frame of a stream, hence a fresh one.
	helloStream := h.openStream()
	send(t, helloStream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", seq, nil))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	// Suspend again. Here a write that skipped deduplication would transition
	// suspended to resuming and call RestoreSnapshot, so the assertions below
	// distinguish deduplication from the state machine's own no-op.
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("第二次 TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

	versionBefore, idleBefore := runRowVersionAndIdle(t, h.pool, runID)

	seq2, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
	if err != nil {
		t.Fatalf("重复 AppendInbox: %v", err)
	}
	if !duplicate || seq2 != seq {
		t.Fatalf("重复 AppendInbox = (%d, %v), 想要 (%d, true)", seq2, duplicate, seq)
	}
	if got := h.callCount(fakeprovider.OpRestore); got != 1 {
		t.Fatalf("Restore 调用次数 = %d, 重复写入不应再次发起恢复", got)
	}
	if state, substate := h.state(runID); state != runcontroller.StateSuspended || substate != runcontroller.SubstateSnapshotted {
		t.Fatalf("重复写入后状态 = %s/%s, 想要 suspended/snapshotted", state, substate)
	}

	versionAfter, idleAfter := runRowVersionAndIdle(t, h.pool, runID)
	if versionAfter != versionBefore {
		t.Fatalf("重复写入后 version = %d, 想要不变（%d）", versionAfter, versionBefore)
	}
	if (idleBefore == nil) != (idleAfter == nil) ||
		(idleBefore != nil && !idleAfter.Equal(*idleBefore)) {
		t.Fatalf("重复写入后 idle_since = %v, 想要不变（%v）", idleAfter, idleBefore)
	}
}

func TestAppendInboxConcurrentDedup(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	ctx := context.Background()
	ev := testSubscriptionEvent("dedup-concurrent")

	// The first writer takes the row lock and waits inside the after-lock hook.
	// The second writer then blocks on the same row lock, so the two lookups
	// run against each other rather than one after the other.
	gate := newLockGate("append")
	h.useGate(t, gate)

	type appendResult struct {
		seq       int64
		duplicate bool
		err       error
	}
	first := make(chan appendResult, 1)
	second := make(chan appendResult, 1)

	go func() {
		seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
		first <- appendResult{seq: seq, duplicate: duplicate, err: err}
	}()
	gate.waitEntered(t)

	go func() {
		seq, duplicate, err := h.ctrl.AppendInbox(ctx, runID, ev)
		second <- appendResult{seq: seq, duplicate: duplicate, err: err}
	}()
	h.waitForLockWaiter()

	gate.open()
	r1 := <-first
	r2 := <-second
	if r1.err != nil || r2.err != nil {
		t.Fatalf("AppendInbox 错误: %v / %v", r1.err, r2.err)
	}
	if r1.seq != r2.seq {
		t.Fatalf("并发写入的序号 = %d 与 %d, 想要相同", r1.seq, r2.seq)
	}
	if r1.seq != 2 {
		t.Fatalf("并发写入的序号 = %d, 想要 2", r1.seq)
	}
	if r1.duplicate == r2.duplicate {
		t.Fatalf("两个并发写入的 duplicate 都是 %v, 想要一个 false 一个 true", r1.duplicate)
	}
	if rows := countInboxDedup(t, h.pool, runID, ev.DedupKey); rows != 1 {
		t.Fatalf("dedup_key 相同的行数 = %d, 想要 1", rows)
	}
	if got := nextInboxSeq(t, h.pool, runID); got != 3 {
		t.Fatalf("并发写入后 next_inbox_seq = %d, 想要 3", got)
	}
}

func TestAppendInboxSubscriptionCompletionWakesSuspended(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

	release := h.provider.Hold(fakeprovider.OpRestore)
	t.Cleanup(release)

	ev := testSubscriptionEvent("wake-1")
	seq, duplicate, err := h.ctrl.AppendInbox(context.Background(), runID, ev)
	if err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	if duplicate || seq != 2 {
		t.Fatalf("AppendInbox = (%d, %v), 想要 (2, false)", seq, duplicate)
	}
	h.waitCalls(fakeprovider.OpRestore, 1)
	release()
	h.waitRestoreReturned(runID)
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
}

func nextInboxSeq(t *testing.T, pool *pgxpool.Pool, runID string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(),
		`SELECT next_inbox_seq FROM run WHERE run_id = $1`, runID).Scan(&seq); err != nil {
		t.Fatalf("查询 next_inbox_seq: %v", err)
	}
	return seq
}

func countInboxDedup(t *testing.T, pool *pgxpool.Pool, runID, dedupKey string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox WHERE run_id = $1 AND dedup_key = $2`, runID, dedupKey).Scan(&n); err != nil {
		t.Fatalf("统计 dedup_key 行数: %v", err)
	}
	return n
}

// runRowVersionAndIdle reads the run columns that a deduplicated write must
// leave untouched.
func runRowVersionAndIdle(t *testing.T, pool *pgxpool.Pool, runID string) (version int64, idleSince *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT version, idle_since FROM run WHERE run_id = $1`, runID).Scan(&version, &idleSince); err != nil {
		t.Fatalf("查询 version 与 idle_since: %v", err)
	}
	return version, idleSince
}
