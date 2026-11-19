package runcontroller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var scanTestEpoch = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func scanTestConfig() Config {
	return Config{
		IdleGrace:      30 * time.Second,
		ScanPeriod:     5 * time.Second,
		HelloTimeout:   60 * time.Second,
		SuspendEnabled: true,
	}
}

// manualClock 是测试文件内的最小手动时钟，不引用 internal/testutil/fakeclock。
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*manualTimer
	created chan struct{}
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now, created: make(chan struct{}, 16)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	t := &manualTimer{clock: c, deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.mu.Unlock()

	select {
	case c.created <- struct{}{}:
	default:
	}
	return t
}

// advance 推进时钟并触发所有到期的计时器。
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	due := make([]*manualTimer, 0, len(c.timers))
	pending := c.timers[:0]
	for _, t := range c.timers {
		if t.deadline.After(now) {
			pending = append(pending, t)
		} else {
			due = append(due, t)
		}
	}
	c.timers = pending
	c.mu.Unlock()

	for _, t := range due {
		t.ch <- now
	}
}

// waitForTimer 等到扫描循环创建了下一个计时器。
func (c *manualClock) waitForTimer(t *testing.T) {
	t.Helper()
	select {
	case <-c.created:
	case <-time.After(5 * time.Second):
		t.Fatal("scan loop did not create a timer")
	}
}

func (c *manualClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

type manualTimer struct {
	clock    *manualClock
	deadline time.Time
	ch       chan time.Time
}

func (t *manualTimer) C() <-chan time.Time {
	return t.ch
}

func (t *manualTimer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.timers {
		if x == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return true
		}
	}
	return false
}

type scanSourceStub struct {
	mu          sync.Mutex
	suspendIDs  []string
	helloIDs    []string
	suspendErr  error
	helloErr    error
	suspendCuts []time.Time
	helloCuts   []time.Time
	suspendN    int
	helloN      int
	events      chan<- string
}

func newScanSourceStub(events chan<- string) *scanSourceStub {
	return &scanSourceStub{events: events}
}

func (s *scanSourceStub) suspendCandidates(_ context.Context, idleBefore time.Time) ([]string, error) {
	s.mu.Lock()
	s.suspendCuts = append(s.suspendCuts, idleBefore)
	s.suspendN++
	ids := append([]string(nil), s.suspendIDs...)
	err := s.suspendErr
	s.mu.Unlock()

	s.events <- "suspend"
	return ids, err
}

func (s *scanSourceStub) helloTimeoutCandidates(_ context.Context, returnedBefore time.Time) ([]string, error) {
	s.mu.Lock()
	s.helloCuts = append(s.helloCuts, returnedBefore)
	s.helloN++
	ids := append([]string(nil), s.helloIDs...)
	err := s.helloErr
	s.mu.Unlock()

	s.events <- "hello"
	return ids, err
}

func (s *scanSourceStub) callCounts() (suspend, hello int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspendN, s.helloN
}

func (s *scanSourceStub) cutoffs() (suspend, hello []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.suspendCuts...), append([]time.Time(nil), s.helloCuts...)
}

type scanTargetStub struct {
	mu     sync.Mutex
	calls  []string
	errs   map[string]error
	events chan<- string
}

func newScanTargetStub(events chan<- string) *scanTargetStub {
	return &scanTargetStub{errs: map[string]error{}, events: events}
}

func (t *scanTargetStub) trySuspend(_ context.Context, runID string) error {
	return t.record("suspend:"+runID, runID)
}

func (t *scanTargetStub) expireHello(_ context.Context, runID string) error {
	return t.record("hello:"+runID, runID)
}

func (t *scanTargetStub) record(call, runID string) error {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	err := t.errs[runID]
	t.mu.Unlock()

	t.events <- call
	return err
}

func (t *scanTargetStub) callList() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

type scanLoopRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startScanLoop(t *testing.T, cfg Config, clock Clock, src scanSource, tgt scanTarget) *scanLoopRun {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	r := &scanLoopRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = runScanLoop(ctx, cfg, clock, src, tgt)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("runScanLoop did not return after cancellation")
		}
	})
	return r
}

func (r *scanLoopRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(5 * time.Second):
		t.Fatal("runScanLoop did not return")
		return nil
	}
}

func expectScanEvents(t *testing.T, ch <-chan string, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-ch:
			if got != w {
				t.Fatalf("scan event = %q, want %q", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for scan event %q", w)
		}
	}
}

func TestScanWaitsForPeriod(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod - time.Second)

	select {
	case e := <-events:
		t.Fatalf("scan ran before a full period: %q", e)
	case <-time.After(50 * time.Millisecond):
	}
	if got := tgt.callList(); len(got) != 0 {
		t.Fatalf("target calls before a full period = %v, want none", got)
	}
	if suspend, hello := src.callCounts(); suspend != 0 || hello != 0 {
		t.Fatalf("source calls before a full period = (%d, %d), want (0, 0)", suspend, hello)
	}

	clock.advance(time.Second)
	expectScanEvents(t, events, "suspend", "hello")

	suspendCuts, helloCuts := src.cutoffs()
	wantSuspend := scanTestEpoch.Add(cfg.ScanPeriod).Add(-cfg.IdleGrace)
	wantHello := scanTestEpoch.Add(cfg.ScanPeriod).Add(-cfg.HelloTimeout)
	if len(suspendCuts) != 1 || !suspendCuts[0].Equal(wantSuspend) {
		t.Fatalf("suspend cutoff = %v, want %v", suspendCuts, wantSuspend)
	}
	if len(helloCuts) != 1 || !helloCuts[0].Equal(wantHello) {
		t.Fatalf("hello cutoff = %v, want %v", helloCuts, wantHello)
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
}

func TestScanCallsCandidatesInOrder(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	src.suspendIDs = []string{"run-1", "run-2"}
	src.helloIDs = []string{"run-3", "run-4"}
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events,
		"suspend", "suspend:run-1", "suspend:run-2",
		"hello", "hello:run-3", "hello:run-4",
	)

	wantCalls := []string{"suspend:run-1", "suspend:run-2", "hello:run-3", "hello:run-4"}
	gotCalls := tgt.callList()
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("target calls = %v, want %v", gotCalls, wantCalls)
	}
	for i := range wantCalls {
		if gotCalls[i] != wantCalls[i] {
			t.Fatalf("target calls = %v, want %v", gotCalls, wantCalls)
		}
	}

	suspendCuts, helloCuts := src.cutoffs()
	wantSuspend := scanTestEpoch.Add(cfg.ScanPeriod).Add(-cfg.IdleGrace)
	wantHello := scanTestEpoch.Add(cfg.ScanPeriod).Add(-cfg.HelloTimeout)
	if len(suspendCuts) != 1 || !suspendCuts[0].Equal(wantSuspend) {
		t.Fatalf("suspend cutoff = %v, want %v", suspendCuts, wantSuspend)
	}
	if len(helloCuts) != 1 || !helloCuts[0].Equal(wantHello) {
		t.Fatalf("hello cutoff = %v, want %v", helloCuts, wantHello)
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
}

func TestScanSuspendDisabledStillChecksHelloTimeout(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	src.suspendIDs = []string{"run-1"}
	src.helloIDs = []string{"run-2"}
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()
	cfg.SuspendEnabled = false
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events, "hello", "hello:run-2")

	if suspend, hello := src.callCounts(); suspend != 0 || hello != 1 {
		t.Fatalf("source calls = (%d, %d), want (0, 1)", suspend, hello)
	}
	wantCalls := []string{"hello:run-2"}
	gotCalls := tgt.callList()
	if len(gotCalls) != 1 || gotCalls[0] != wantCalls[0] {
		t.Fatalf("target calls = %v, want %v", gotCalls, wantCalls)
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
}

func TestScanRunErrorDoesNotStopRound(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	src.suspendIDs = []string{"bad", "good-1", "good-2"}
	src.helloIDs = []string{"late"}
	tgt := newScanTargetStub(events)
	tgt.errs["bad"] = errors.New("boom")
	cfg := scanTestConfig()
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events,
		"suspend", "suspend:bad", "suspend:good-1", "suspend:good-2",
		"hello", "hello:late",
	)

	wantCalls := []string{"suspend:bad", "suspend:good-1", "suspend:good-2", "hello:late"}
	gotCalls := tgt.callList()
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("target calls = %v, want %v", gotCalls, wantCalls)
	}
	for i := range wantCalls {
		if gotCalls[i] != wantCalls[i] {
			t.Fatalf("target calls = %v, want %v", gotCalls, wantCalls)
		}
	}

	// 下一轮照常执行，单个运行实例的错误不会终止扫描循环。
	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events,
		"suspend", "suspend:bad", "suspend:good-1", "suspend:good-2",
		"hello", "hello:late",
	)

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
}

func TestScanCandidateQueryErrorStillChecksOtherKind(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	src.suspendErr = errors.New("query failed")
	src.helloIDs = []string{"run-2"}
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events, "suspend", "hello", "hello:run-2")

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
}

func TestScanStopsOnCancel(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()
	run := startScanLoop(t, cfg, clock, src, tgt)

	clock.waitForTimer(t)
	clock.advance(cfg.ScanPeriod)
	expectScanEvents(t, events, "suspend", "hello")

	clock.waitForTimer(t)
	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
	if got := clock.timerCount(); got != 0 {
		t.Fatalf("pending timers after cancel = %d, want 0", got)
	}

	clock.advance(cfg.ScanPeriod)
	select {
	case e := <-events:
		t.Fatalf("scan ran after cancellation: %q", e)
	case <-time.After(50 * time.Millisecond):
	}
	if got := tgt.callList(); len(got) != 0 {
		t.Fatalf("target calls after cancellation = %v, want none", got)
	}
}

func TestScanAlreadyCanceled(t *testing.T) {
	t.Parallel()

	clock := newManualClock(scanTestEpoch)
	events := make(chan string, 64)
	src := newScanSourceStub(events)
	tgt := newScanTargetStub(events)
	cfg := scanTestConfig()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runScanLoop(ctx, cfg, clock, src, tgt); err != nil {
		t.Fatalf("runScanLoop() = %v, want nil", err)
	}
	if got := clock.timerCount(); got != 0 {
		t.Fatalf("timers created = %d, want 0", got)
	}
	if suspend, hello := src.callCounts(); suspend != 0 || hello != 0 {
		t.Fatalf("source calls = (%d, %d), want (0, 0)", suspend, hello)
	}
	if got := tgt.callList(); len(got) != 0 {
		t.Fatalf("target calls = %v, want none", got)
	}
}
