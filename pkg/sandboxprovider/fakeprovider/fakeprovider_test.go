package fakeprovider_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// recordingTB is a testing.TB that records Errorf calls instead of failing the
// surrounding test. It is safe for concurrent use.
type recordingTB struct {
	testing.TB

	mu   sync.Mutex
	msgs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

func (r *recordingTB) hasMessage(substr string) bool {
	for _, msg := range r.messages() {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

func TestProviderImplementsInterface(t *testing.T) {
	provider := fakeprovider.New(t)
	if provider == nil {
		t.Fatal("New returned nil")
	}
	var _ sandboxprovider.Provider = provider
}

func TestLifecycle(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")

	timing, err := p.Provision(ctx, "run-1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if timing.Duration < 0 {
		t.Fatalf("Provision duration = %v, want >= 0", timing.Duration)
	}
	if timing.SnapshotBytes != 0 {
		t.Fatalf("Provision SnapshotBytes = %d, want 0", timing.SnapshotBytes)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateRunning)

	timing, err = p.Snapshot(ctx, actor)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if timing.SnapshotBytes <= 0 {
		t.Fatalf("Snapshot SnapshotBytes = %d, want > 0", timing.SnapshotBytes)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateSuspended)

	if _, err := p.RestoreSnapshot(ctx, actor); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateRunning)

	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := p.GetSandbox(ctx, actor); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("GetSandbox after Destroy = %v, want ErrNotFound", err)
	}
	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy nonexistent actor = %v, want nil", err)
	}
}

func assertState(t *testing.T, p *fakeprovider.Provider, actor string, want sandboxprovider.ActorState) {
	t.Helper()
	status, err := p.GetSandbox(context.Background(), actor)
	if err != nil {
		t.Fatalf("GetSandbox(%q): %v", actor, err)
	}
	if status.State != want {
		t.Fatalf("GetSandbox(%q).State = %q, want %q", actor, status.State, want)
	}
}

func TestCalls(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")

	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := p.Snapshot(ctx, actor); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := p.RestoreSnapshot(ctx, actor); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := p.GetSandbox(ctx, actor); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("GetSandbox: %v, want ErrNotFound", err)
	}

	cases := []struct {
		op   fakeprovider.Op
		want []string
	}{
		{fakeprovider.OpProvision, []string{actor}},
		{fakeprovider.OpSnapshot, []string{actor}},
		{fakeprovider.OpRestore, []string{actor}},
		{fakeprovider.OpDestroy, []string{actor}},
		{fakeprovider.OpGetSandbox, []string{actor}},
	}
	for _, tc := range cases {
		got := p.Calls(tc.op)
		if len(got) != len(tc.want) {
			t.Fatalf("Calls(%s) = %v, want %v", tc.op, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("Calls(%s)[%d] = %q, want %q", tc.op, i, got[i], tc.want[i])
			}
		}
	}
}

func TestCallsReturnsCopy(t *testing.T) {
	p := fakeprovider.New(t)
	if _, err := p.Provision(context.Background(), "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := p.Calls(fakeprovider.OpProvision)
	got[0] = "mutated"
	if again := p.Calls(fakeprovider.OpProvision); again[0] != sandboxprovider.ActorName("run-1") {
		t.Fatalf("Calls returned shared storage: %v", again)
	}
}

func TestHoldAndWaitForCalls(t *testing.T) {
	p := fakeprovider.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	release := p.Hold(fakeprovider.OpProvision)
	done := make(chan error, 1)
	go func() {
		_, err := p.Provision(context.Background(), "run-1")
		done <- err
	}()

	if err := p.WaitForCalls(ctx, fakeprovider.OpProvision, 1); err != nil {
		t.Fatalf("WaitForCalls: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Provision returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Provision did not return after release")
	}
}

func TestWaitForCallsAlreadyStarted(t *testing.T) {
	p := fakeprovider.New(t)
	if _, err := p.Provision(context.Background(), "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.WaitForCalls(ctx, fakeprovider.OpProvision, 1); err != nil {
		t.Fatalf("WaitForCalls on an already started call: %v", err)
	}
}

func TestWaitForCallsHonorsContext(t *testing.T) {
	p := fakeprovider.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.WaitForCalls(ctx, fakeprovider.OpProvision, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForCalls = %v, want context.DeadlineExceeded", err)
	}
}

func TestSetDelay(t *testing.T) {
	p := fakeprovider.New(t)
	const delay = 50 * time.Millisecond
	p.SetDelay(fakeprovider.OpProvision, delay)

	start := time.Now()
	timing, err := p.Provision(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("Provision returned after %v, want at least %v", elapsed, delay)
	}
	if timing.Duration < delay {
		t.Fatalf("Timing.Duration = %v, want at least %v", timing.Duration, delay)
	}
}

func TestSetDelayCanceledByContext(t *testing.T) {
	p := fakeprovider.New(t)
	p.SetDelay(fakeprovider.OpProvision, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.Provision(ctx, "run-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Provision = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("Provision took %v, want early return on cancellation", elapsed)
	}

	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.GetSandbox(context.Background(), actor); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("GetSandbox after canceled Provision = %v, want ErrNotFound", err)
	}
}

func TestFailNext(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	boom := errors.New("injected snapshot failure")
	p.FailNext(fakeprovider.OpSnapshot, boom)

	if _, err := p.Snapshot(ctx, actor); !errors.Is(err, boom) {
		t.Fatalf("Snapshot = %v, want injected error", err)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateRunning)

	if _, err := p.Snapshot(ctx, actor); err != nil {
		t.Fatalf("Snapshot after injected failure: %v", err)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateSuspended)
}

func TestFailNextQueues(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	first := errors.New("first")
	second := errors.New("second")
	p.FailNext(fakeprovider.OpProvision, first)
	p.FailNext(fakeprovider.OpProvision, second)

	if _, err := p.Provision(ctx, "run-1"); !errors.Is(err, first) {
		t.Fatalf("Provision = %v, want first", err)
	}
	if _, err := p.Provision(ctx, "run-1"); !errors.Is(err, second) {
		t.Fatalf("Provision = %v, want second", err)
	}
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision after queue drained: %v", err)
	}
}

func TestFailNextDestroy(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	boom := errors.New("injected destroy failure")
	p.FailNext(fakeprovider.OpDestroy, boom)
	if err := p.Destroy(ctx, actor); !errors.Is(err, boom) {
		t.Fatalf("Destroy = %v, want injected error", err)
	}
	assertState(t, p, actor, sandboxprovider.ActorStateRunning)

	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy after injected failure: %v", err)
	}
	if _, err := p.GetSandbox(ctx, actor); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("GetSandbox after Destroy = %v, want ErrNotFound", err)
	}
}

func TestSetCrashed(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	p.SetCrashed(actor)
	assertState(t, p, actor, sandboxprovider.ActorStateCrashed)

	if _, err := p.RestoreSnapshot(ctx, actor); !errors.Is(err, sandboxprovider.ErrActorCrashed) {
		t.Fatalf("RestoreSnapshot = %v, want ErrActorCrashed", err)
	}
	if _, err := p.Provision(ctx, "run-1"); !errors.Is(err, sandboxprovider.ErrActorCrashed) {
		t.Fatalf("Provision on crashed actor = %v, want ErrActorCrashed", err)
	}
}

func TestSetCrashedUnknownActor(t *testing.T) {
	p := fakeprovider.New(t)
	p.SetCrashed("cw-ghost")
	status, err := p.GetSandbox(context.Background(), "cw-ghost")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if status.State != sandboxprovider.ActorStateCrashed {
		t.Fatalf("State = %q, want %q", status.State, sandboxprovider.ActorStateCrashed)
	}
}

func TestCallbacks(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")

	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	p.OnProvisioned(func(runID string) { record("provisioned:" + runID) })
	p.OnSnapshotted(func(a string) { record("snapshotted:" + a) })
	p.OnRestored(func(a string) { record("restored:" + a) })
	p.OnDestroyed(func(a string) { record("destroyed:" + a) })

	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := p.Snapshot(ctx, actor); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := p.RestoreSnapshot(ctx, actor); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []string{
		"provisioned:run-1",
		"snapshotted:" + actor,
		"restored:" + actor,
		"destroyed:" + actor,
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestCallbacksNotCalledOnFailure(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	called := false
	p.OnSnapshotted(func(string) { called = true })
	boom := errors.New("boom")
	p.FailNext(fakeprovider.OpSnapshot, boom)
	if _, err := p.Snapshot(ctx, actor); !errors.Is(err, boom) {
		t.Fatalf("Snapshot = %v, want injected error", err)
	}
	if called {
		t.Fatal("OnSnapshotted called after a failed Snapshot")
	}
}

func TestDestroyCallbackOnNonexistent(t *testing.T) {
	p := fakeprovider.New(t)
	var destroyed []string
	p.OnDestroyed(func(actor string) { destroyed = append(destroyed, actor) })

	if err := p.Destroy(context.Background(), "cw-ghost"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(destroyed) != 1 || destroyed[0] != "cw-ghost" {
		t.Fatalf("OnDestroyed calls = %v, want [cw-ghost]", destroyed)
	}
}

func TestViolationOverlappingCalls(t *testing.T) {
	tb := &recordingTB{}
	p := fakeprovider.New(tb)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	release := p.Hold(fakeprovider.OpSnapshot)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Snapshot(ctx, actor)
		}()
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.WaitForCalls(waitCtx, fakeprovider.OpSnapshot, 2); err != nil {
		t.Fatalf("WaitForCalls: %v", err)
	}
	release()
	wg.Wait()

	if !tb.hasMessage("overlapping calls") {
		t.Fatalf("no overlap violation reported, messages: %v", tb.messages())
	}
}

func TestViolationSnapshotOnNonexistent(t *testing.T) {
	tb := &recordingTB{}
	p := fakeprovider.New(tb)

	if _, err := p.Snapshot(context.Background(), "cw-ghost"); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("Snapshot = %v, want ErrNotFound", err)
	}
	if !tb.hasMessage("nonexistent actor") {
		t.Fatalf("no violation reported, messages: %v", tb.messages())
	}
}

func TestViolationSnapshotOnSuspended(t *testing.T) {
	tb := &recordingTB{}
	p := fakeprovider.New(tb)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := p.Snapshot(ctx, actor); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(tb.messages()) != 0 {
		t.Fatalf("unexpected violation before snapshot on suspended actor: %v", tb.messages())
	}

	if _, err := p.Snapshot(ctx, actor); err == nil {
		t.Fatal("Snapshot on suspended actor returned nil error")
	}
	if !tb.hasMessage("suspended actor") {
		t.Fatalf("no violation reported, messages: %v", tb.messages())
	}
}

func TestViolationRestoreOnRunning(t *testing.T) {
	tb := &recordingTB{}
	p := fakeprovider.New(tb)
	ctx := context.Background()
	actor := sandboxprovider.ActorName("run-1")
	if _, err := p.Provision(ctx, "run-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if _, err := p.RestoreSnapshot(ctx, actor); err == nil {
		t.Fatal("RestoreSnapshot on running actor returned nil error")
	}
	if !tb.hasMessage("running actor") {
		t.Fatalf("no violation reported, messages: %v", tb.messages())
	}
}

func TestViolationRestoreOnNonexistent(t *testing.T) {
	tb := &recordingTB{}
	p := fakeprovider.New(tb)

	if _, err := p.RestoreSnapshot(context.Background(), "cw-ghost"); !errors.Is(err, sandboxprovider.ErrNotFound) {
		t.Fatalf("RestoreSnapshot = %v, want ErrNotFound", err)
	}
	if !tb.hasMessage("nonexistent actor") {
		t.Fatalf("no violation reported, messages: %v", tb.messages())
	}
}

func TestConcurrentActors(t *testing.T) {
	p := fakeprovider.New(t)
	ctx := context.Background()

	const actors = 16
	var wg sync.WaitGroup
	for i := 0; i < actors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", i)
			actor := sandboxprovider.ActorName(runID)

			if _, err := p.Provision(ctx, runID); err != nil {
				t.Errorf("Provision(%s): %v", runID, err)
				return
			}
			if _, err := p.Snapshot(ctx, actor); err != nil {
				t.Errorf("Snapshot(%s): %v", actor, err)
				return
			}
			if _, err := p.RestoreSnapshot(ctx, actor); err != nil {
				t.Errorf("RestoreSnapshot(%s): %v", actor, err)
				return
			}
			status, err := p.GetSandbox(ctx, actor)
			if err != nil {
				t.Errorf("GetSandbox(%s): %v", actor, err)
				return
			}
			if status.State != sandboxprovider.ActorStateRunning {
				t.Errorf("GetSandbox(%s).State = %q, want running", actor, status.State)
			}
			if err := p.Destroy(ctx, actor); err != nil {
				t.Errorf("Destroy(%s): %v", actor, err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(p.Calls(fakeprovider.OpProvision)); got != actors {
		t.Fatalf("Provision calls = %d, want %d", got, actors)
	}
}
