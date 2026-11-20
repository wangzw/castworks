package connectorhub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// fakeDeliveryRuns 是 deliveryRuns 的测试替身。
type fakeDeliveryRuns struct {
	mu        sync.Mutex
	details   map[string]runcontroller.RunDetail
	getErrors map[string]error
	appendFn  func(context.Context, string, Completion) (bool, error)
	received  []Completion
}

func newFakeDeliveryRuns() *fakeDeliveryRuns {
	return &fakeDeliveryRuns{
		details:   map[string]runcontroller.RunDetail{},
		getErrors: map[string]error{},
	}
}

func (f *fakeDeliveryRuns) Get(_ context.Context, runID string) (runcontroller.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.getErrors[runID]; err != nil {
		return runcontroller.RunDetail{}, err
	}
	if detail, ok := f.details[runID]; ok {
		return detail, nil
	}
	return runcontroller.RunDetail{}, runcontroller.ErrNotFound
}

func (f *fakeDeliveryRuns) AppendCompletion(ctx context.Context, runID string, c Completion) (bool, error) {
	f.mu.Lock()
	f.received = append(f.received, c)
	fn := f.appendFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, runID, c)
	}
	return false, nil
}

func (f *fakeDeliveryRuns) receivedCompletions() []Completion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Completion(nil), f.received...)
}

func testCompletion(subID string) Completion {
	return Completion{
		SubID:              subID,
		System:             "extsim",
		Event:              "job.completed",
		SubscriptionStatus: "completed",
		Terminal:           "succeeded",
		Correlation:        map[string]string{"job.id": "j-1"},
		OccurredAt:         testNow().Add(-time.Minute),
		Source:             "webhook",
		Detail:             testJobPayload,
		EventID:            encodeTuple(subID, "succeeded"),
		DedupKey:           hashTuple(subID, "delivery-"+subID),
	}
}

func insertTestCompletion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID string, c Completion) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin completion insert: %v", err)
	}
	if err := insertCompletionDelivery(ctx, tx, runID, c, testNow()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert completion for %s: %v", c.SubID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit completion insert for %s: %v", c.SubID, err)
	}
}

type testDeliveryState struct {
	Status      string
	Attempts    int
	LastError   string
	DeliveredAt *time.Time
}

func readTestDeliveryState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subID, dedupKey string) testDeliveryState {
	t.Helper()
	var state testDeliveryState
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts, last_error, delivered_at
		   FROM completion_delivery WHERE sub_id = $1 AND dedup_key = $2`, subID, dedupKey,
	).Scan(&state.Status, &state.Attempts, &state.LastError, &state.DeliveredAt); err != nil {
		t.Fatalf("read completion %s/%s: %v", subID, dedupKey, err)
	}
	return state
}

func waitTestDeliveryStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subID, dedupKey, want string) testDeliveryState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := readTestDeliveryState(t, ctx, pool, subID, dedupKey)
		if state.Status == want {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion %s stayed %s, want %s", subID, state.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runDetail(state runcontroller.State) runcontroller.RunDetail {
	return runcontroller.RunDetail{RunSummary: runcontroller.RunSummary{State: state}}
}

func TestDeliverPendingMarksDelivered(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	runs := newFakeDeliveryRuns()
	d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

	completion := testCompletion("sub-1")
	insertTestCompletion(t, ctx, pool, "run-1", completion)

	if err := d.deliverPending(ctx); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	state := readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
	if state.Status != "delivered" {
		t.Fatalf("status = %q, want delivered", state.Status)
	}
	if state.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.Attempts)
	}
	if state.LastError != "" {
		t.Fatalf("last_error = %q, want empty", state.LastError)
	}
	if state.DeliveredAt == nil || !state.DeliveredAt.Equal(now) {
		t.Fatalf("delivered_at = %v, want %v", state.DeliveredAt, now)
	}

	received := runs.receivedCompletions()
	if len(received) != 1 {
		t.Fatalf("AppendCompletion calls = %d, want 1", len(received))
	}
	if received[0].SubID != completion.SubID || received[0].DedupKey != completion.DedupKey {
		t.Fatalf("AppendCompletion got %+v, want %+v", received[0], completion)
	}
}

func TestDeliverPendingTreatsDuplicateAsDelivered(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	runs := newFakeDeliveryRuns()
	runs.appendFn = func(context.Context, string, Completion) (bool, error) {
		return true, nil
	}
	d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

	completion := testCompletion("sub-1")
	insertTestCompletion(t, ctx, pool, "run-1", completion)

	if err := d.deliverPending(ctx); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	state := readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
	if state.Status != "delivered" {
		t.Fatalf("status = %q, want delivered for a duplicate", state.Status)
	}
	if state.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.Attempts)
	}
}

func TestDeliverPendingRejectsTerminalErrors(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	tests := []struct {
		name string
		err  error
	}{
		{name: "run not found", err: runcontroller.ErrNotFound},
		{name: "run terminated", err: fmt.Errorf("append inbox: %w", runcontroller.ErrTerminated)},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := testNow().Add(time.Duration(i) * time.Minute)
			subID := fmt.Sprintf("sub-reject-%d", i)
			runs := newFakeDeliveryRuns()
			runs.appendFn = func(context.Context, string, Completion) (bool, error) {
				return false, tc.err
			}
			d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

			sub := defaultTestSubscription(subID)
			sub.RunID = "run-1"
			insertTestSubscription(t, ctx, pool, sub)
			completion := testCompletion(subID)
			insertTestCompletion(t, ctx, pool, sub.RunID, completion)

			if err := d.deliverPending(ctx); err != nil {
				t.Fatalf("deliverPending: %v", err)
			}

			state := readTestDeliveryState(t, ctx, pool, subID, completion.DedupKey)
			if state.Status != "rejected" {
				t.Fatalf("status = %q, want rejected", state.Status)
			}
			if state.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", state.Attempts)
			}
			if state.DeliveredAt == nil || !state.DeliveredAt.Equal(now) {
				t.Fatalf("delivered_at = %v, want %v", state.DeliveredAt, now)
			}

			status, _, completedAt := readTestSubscription(t, ctx, pool, subID)
			if status != "cancelled" {
				t.Fatalf("subscription status = %q, want cancelled after rejected delivery", status)
			}
			if completedAt == nil || !completedAt.Equal(now) {
				t.Fatalf("subscription completed_at = %v, want %v", completedAt, now)
			}
		})
	}
}

func TestDeliverPendingRetriesTransientError(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()
	var attempts atomic.Int32
	runs := newFakeDeliveryRuns()
	runs.appendFn = func(context.Context, string, Completion) (bool, error) {
		if attempts.Add(1) == 1 {
			return false, errors.New("inbox temporarily unavailable")
		}
		return false, nil
	}
	d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

	completion := testCompletion("sub-1")
	insertTestCompletion(t, ctx, pool, "run-1", completion)

	if err := d.deliverPending(ctx); err != nil {
		t.Fatalf("first deliverPending: %v", err)
	}

	state := readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
	if state.Status != "pending" {
		t.Fatalf("status after transient error = %q, want pending", state.Status)
	}
	if state.Attempts != 1 {
		t.Fatalf("attempts after transient error = %d, want 1", state.Attempts)
	}
	if state.LastError != "inbox temporarily unavailable" {
		t.Fatalf("last_error = %q, want the append error", state.LastError)
	}

	if err := d.deliverPending(ctx); err != nil {
		t.Fatalf("second deliverPending: %v", err)
	}

	state = readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
	if state.Status != "delivered" {
		t.Fatalf("status after retry = %q, want delivered", state.Status)
	}
	if state.Attempts != 2 {
		t.Fatalf("attempts after retry = %d, want 2", state.Attempts)
	}
	if state.LastError != "" {
		t.Fatalf("last_error after success = %q, want empty", state.LastError)
	}
}

func TestScanCancelsSubscriptionsOfTerminatedRuns(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testNow()

	endedAt := now.Add(-time.Hour)
	subs := []subscription{
		defaultTestSubscription("sub-ended"),
		defaultTestSubscription("sub-failed"),
		defaultTestSubscription("sub-missing"),
		defaultTestSubscription("sub-live"),
		defaultTestSubscription("sub-done"),
	}
	subs[0].RunID = "run-ended"
	subs[1].RunID = "run-failed"
	subs[2].RunID = "run-missing"
	subs[3].RunID = "run-live"
	subs[3].Correlation = map[string]string{"job.id": "j-2"}
	subs[4].RunID = "run-ended"
	subs[4].Status = "completed"
	subs[4].Terminal = "succeeded"
	subs[4].CompletedAt = &endedAt
	for _, sub := range subs {
		insertTestSubscription(t, ctx, pool, sub)
	}

	runs := newFakeDeliveryRuns()
	runs.details["run-ended"] = runDetail(runcontroller.StateEnded)
	runs.details["run-failed"] = runDetail(runcontroller.StateFailed)
	runs.details["run-live"] = runDetail(runcontroller.StateActive)
	runs.getErrors["run-missing"] = runcontroller.ErrNotFound
	d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

	if err := d.cancelTerminated(ctx); err != nil {
		t.Fatalf("cancelTerminated: %v", err)
	}

	for _, subID := range []string{"sub-ended", "sub-failed", "sub-missing"} {
		status, _, completedAt := readTestSubscription(t, ctx, pool, subID)
		if status != "cancelled" {
			t.Fatalf("%s status = %q, want cancelled", subID, status)
		}
		if completedAt == nil || !completedAt.Equal(now) {
			t.Fatalf("%s completed_at = %v, want %v", subID, completedAt, now)
		}
	}

	status, _, completedAt := readTestSubscription(t, ctx, pool, "sub-live")
	if status != "active" {
		t.Fatalf("sub-live status = %q, want active", status)
	}
	if completedAt != nil {
		t.Fatalf("sub-live completed_at = %v, want nil", completedAt)
	}

	status, terminal, completedAt := readTestSubscription(t, ctx, pool, "sub-done")
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("sub-done = status %q terminal %q, want completed/succeeded", status, terminal)
	}
	if completedAt == nil || !completedAt.Equal(endedAt) {
		t.Fatalf("sub-done completed_at = %v, want %v", completedAt, endedAt)
	}

	// 取消之后到达的匹配事件不再产生投递。
	ev := defaultTestEvent(1, "d-after-cancel", now)
	insertTestEvent(t, ctx, pool, ev)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin match: %v", err)
	}
	completed, err := matchEvent(ctx, tx, ev, jobConclusionOrFail(t, ev.Payload), now)
	if err != nil {
		t.Fatalf("matchEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit match: %v", err)
	}
	if completed != 0 {
		t.Fatalf("matchEvent completed %d subscriptions after cancellation, want 0", completed)
	}
	for _, subID := range []string{"sub-ended", "sub-failed", "sub-missing", "sub-live"} {
		if got := countTestRows(t, ctx, pool, "completion_delivery", subID); got != 0 {
			t.Fatalf("completion_delivery rows for %s after cancellation = %d, want 0", subID, got)
		}
	}
}

func TestRunScansAtStartupAndReturnsOnCancel(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	now := testNow()
	runs := newFakeDeliveryRuns()
	// 周期设为一小时，投递只可能来自启动时的那一次扫描。
	d := newDeliverer(pool, runs, time.Hour, func() time.Time { return now })

	completion := testCompletion("sub-1")
	insertTestCompletion(t, t.Context(), pool, "run-1", completion)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.run(ctx) }()

	state := waitTestDeliveryStatus(t, t.Context(), pool, completion.SubID, completion.DedupKey, "delivered")
	if state.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.Attempts)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

func TestRunScanRepeatsEveryPeriod(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	now := testNow()
	var attempts atomic.Int32
	runs := newFakeDeliveryRuns()
	runs.appendFn = func(context.Context, string, Completion) (bool, error) {
		if attempts.Add(1) == 1 {
			return false, errors.New("inbox temporarily unavailable")
		}
		return false, nil
	}
	d := newDeliverer(pool, runs, 20*time.Millisecond, func() time.Time { return now })

	completion := testCompletion("sub-1")
	insertTestCompletion(t, t.Context(), pool, "run-1", completion)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.run(ctx) }()

	state := waitTestDeliveryStatus(t, t.Context(), pool, completion.SubID, completion.DedupKey, "delivered")
	if state.Attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2 after a transient failure", state.Attempts)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}
