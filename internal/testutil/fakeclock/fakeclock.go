// Package fakeclock provides a manually advanced clock for tests.
//
// It implements runcontroller.Clock: Now returns the time set by New and
// Advance, and NewTimer returns a timer that fires on Advance instead of when
// wall time passes. Tests that need to drive a component through a timeout
// create the timers, wait for them with WaitForTimers and then call Advance.
//
// A Clock and the timers it creates are safe for concurrent use.
package fakeclock

import (
	"context"
	"sync"
	"time"

	"github.com/wangzw/castworks/internal/runcontroller"
)

var _ runcontroller.Clock = (*Clock)(nil)

// Clock is a runcontroller.Clock whose time only moves when Advance is called.
//
// The zero value is not usable; call New.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*Timer
	// notify is closed and replaced whenever the number of active timers
	// changes, waking every goroutine blocked in WaitForTimers.
	notify chan struct{}
}

// Timer is a timer created by Clock.
type Timer struct {
	clock    *Clock
	deadline time.Time
	ch       chan time.Time
	fired    bool
	stopped  bool
}

// New returns a Clock reading start.
func New(start time.Time) *Clock {
	return &Clock{
		now:    start,
		notify: make(chan struct{}),
	}
}

// Now returns the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer that fires on the first Advance that moves the
// clock to or past d from now. A non-positive d is already due, so the timer
// fires at once.
//
// The timer's channel has a buffer of one, so firing never blocks.
func (c *Clock) NewTimer(d time.Duration) runcontroller.Timer {
	c.mu.Lock()
	now := c.now
	t := &Timer{
		clock:    c,
		deadline: now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	if !t.deadline.After(now) {
		t.fired = true
	} else {
		c.timers = append(c.timers, t)
	}
	c.wake()
	c.mu.Unlock()

	if t.fired {
		// The channel is fresh and buffered, so this send cannot block.
		t.ch <- now
	}
	return t
}

// Advance moves the clock forward by d and fires every timer whose deadline is
// now reached. Each due timer is fired exactly once, with the advanced time on
// its channel. Timers that are stopped or already fired do not fire again.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var fired []*Timer
	active := c.timers[:0]
	for _, t := range c.timers {
		if !t.deadline.After(now) {
			t.fired = true
			fired = append(fired, t)
			continue
		}
		active = append(active, t)
	}
	c.timers = active
	if len(fired) > 0 {
		c.wake()
	}
	c.mu.Unlock()

	for _, t := range fired {
		// The channel is buffered and the timer fires only once, so the send
		// never blocks; the select guards against a caller that replaced the
		// buffer in a way that leaves it full.
		select {
		case t.ch <- now:
		default:
		}
	}
}

// WaitForTimers blocks until at least n timers created by c are active, that
// is, neither fired nor stopped. It returns ctx.Err() if ctx ends first.
func (c *Clock) WaitForTimers(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		if len(c.timers) >= n {
			c.mu.Unlock()
			return nil
		}
		notify := c.notify
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

// C returns the channel on which the timer fires.
func (t *Timer) C() <-chan time.Time {
	return t.ch
}

// Stop prevents the timer from firing. It reports whether the call stopped the
// timer, with the semantics of time.Timer.Stop: it returns false if the timer
// has already fired or has already been stopped.
func (t *Timer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()

	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	c.remove(t)
	c.wake()
	return true
}

// remove deletes t from the active list. The caller must hold c.mu.
func (c *Clock) remove(t *Timer) {
	for i, other := range c.timers {
		if other == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return
		}
	}
}

// wake closes the current notification channel and installs a fresh one so
// goroutines in WaitForTimers re-check the active timer count. The caller must
// hold c.mu.
func (c *Clock) wake() {
	close(c.notify)
	c.notify = make(chan struct{})
}
