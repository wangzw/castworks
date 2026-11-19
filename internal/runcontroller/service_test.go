package runcontroller

import (
	"testing"
	"time"
)

func TestRealClockNow(t *testing.T) {
	clock := RealClock()

	before := time.Now()
	got := clock.Now()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestRealClockNewTimerFires(t *testing.T) {
	clock := RealClock()
	timer := clock.NewTimer(10 * time.Millisecond)

	select {
	case <-timer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not fire")
	}
}

func TestRealClockTimerStop(t *testing.T) {
	t.Run("stops an unfired timer", func(t *testing.T) {
		timer := RealClock().NewTimer(time.Hour)
		if !timer.Stop() {
			t.Fatal("Stop() = false, want true for an unfired timer")
		}
	})

	t.Run("second Stop returns false", func(t *testing.T) {
		timer := RealClock().NewTimer(time.Hour)
		if !timer.Stop() {
			t.Fatal("first Stop() = false, want true")
		}
		if timer.Stop() {
			t.Fatal("second Stop() = true, want false")
		}
	})

	t.Run("Stop after firing returns false", func(t *testing.T) {
		timer := RealClock().NewTimer(10 * time.Millisecond)

		select {
		case <-timer.C():
		case <-time.After(5 * time.Second):
			t.Fatal("timer did not fire")
		}

		if timer.Stop() {
			t.Fatal("Stop() = true after the timer fired, want false")
		}
	})
}
