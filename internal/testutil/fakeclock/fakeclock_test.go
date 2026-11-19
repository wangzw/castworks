package fakeclock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// recv returns the time sent on ch, failing the test if nothing arrives.
func recv(t *testing.T, ch <-chan time.Time) time.Time {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not fire")
		return time.Time{}
	}
}

// recvNothing fails the test if ch delivers a value.
func recvNothing(t *testing.T, ch <-chan time.Time) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("timer fired unexpectedly at %v", got)
	default:
	}
}

func TestNow(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	if got := clock.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v, want %v", got, base)
	}
	clock.Advance(3 * time.Second)
	if got := clock.Now(); !got.Equal(base.Add(3 * time.Second)) {
		t.Fatalf("Now() after Advance = %v, want %v", got, base.Add(3*time.Second))
	}
}

func TestTimerFiresAtDeadlineOnlyOnce(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	timer := clock.NewTimer(5 * time.Second)

	clock.Advance(4 * time.Second)
	recvNothing(t, timer.C())

	clock.Advance(time.Second)
	if got, want := recv(t, timer.C()), base.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("timer fired with %v, want %v", got, want)
	}

	clock.Advance(time.Hour)
	recvNothing(t, timer.C())
}

func TestAdvanceFiresAllDueTimers(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	early := clock.NewTimer(time.Second)
	late := clock.NewTimer(3 * time.Second)

	clock.Advance(2 * time.Second)
	if got, want := recv(t, early.C()), base.Add(2*time.Second); !got.Equal(want) {
		t.Fatalf("early timer fired with %v, want %v", got, want)
	}
	recvNothing(t, late.C())

	clock.Advance(time.Second)
	if got, want := recv(t, late.C()), base.Add(3*time.Second); !got.Equal(want) {
		t.Fatalf("late timer fired with %v, want %v", got, want)
	}
}

func TestNonPositiveTimerFiresImmediately(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	timer := clock.NewTimer(0)
	if got, want := recv(t, timer.C()), base; !got.Equal(want) {
		t.Fatalf("timer fired with %v, want %v", got, want)
	}
	if timer.Stop() {
		t.Fatal("Stop after firing = true, want false")
	}
}

func TestStopBeforeDeadline(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	timer := clock.NewTimer(time.Second)

	if !timer.Stop() {
		t.Fatal("Stop before deadline = false, want true")
	}
	if timer.Stop() {
		t.Fatal("second Stop = true, want false")
	}

	clock.Advance(time.Hour)
	recvNothing(t, timer.C())
}

func TestStopAfterFiring(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	timer := clock.NewTimer(time.Second)
	clock.Advance(time.Second)
	recv(t, timer.C())

	if timer.Stop() {
		t.Fatal("Stop after firing = true, want false")
	}
}

func TestWaitForTimersWaitsForActiveTimer(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- clock.WaitForTimers(ctx, 1)
	}()

	select {
	case err := <-done:
		t.Fatalf("WaitForTimers returned before a timer existed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	timer := clock.NewTimer(time.Second)
	defer timer.Stop()
	if err := <-done; err != nil {
		t.Fatalf("WaitForTimers = %v, want nil", err)
	}
}

func TestWaitForTimersIgnoresStoppedAndFired(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	timer := clock.NewTimer(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := clock.WaitForTimers(ctx, 1); err != nil {
		t.Fatalf("WaitForTimers with one active timer = %v, want nil", err)
	}

	timer.Stop()
	waitTimedOut(t, clock)

	other := clock.NewTimer(time.Second)
	clock.Advance(time.Second)
	recv(t, other.C())
	waitTimedOut(t, clock)
}

// waitTimedOut requires WaitForTimers(ctx, 1) to block until ctx ends, which
// shows that no untriggered, unstopped timer remains.
func waitTimedOut(t *testing.T, clock *fakeclock.Clock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := clock.WaitForTimers(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForTimers = %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestWaitForTimersContextCanceled(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := clock.WaitForTimers(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForTimers = %v, want %v", err, context.Canceled)
	}
}

func TestZeroTimersNeeded(t *testing.T) {
	t.Parallel()

	if err := fakeclock.New(base).WaitForTimers(context.Background(), 0); err != nil {
		t.Fatalf("WaitForTimers(0) = %v, want nil", err)
	}
}

// TestConcurrentUse runs Advance, NewTimer, Stop and WaitForTimers together to
// let the race detector check the locking.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	clock := fakeclock.New(base)
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(4)

		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				clock.Advance(time.Millisecond)
			}
		}()

		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				clock.NewTimer(time.Second)
			}
		}()

		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				timer := clock.NewTimer(time.Second)
				if j%2 == 0 {
					timer.Stop()
				}
			}
		}()

		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
				_ = clock.WaitForTimers(ctx, 2)
				cancel()
			}
		}()
	}

	wg.Wait()
}
