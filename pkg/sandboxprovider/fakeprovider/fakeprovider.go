// Package fakeprovider provides a controllable test double for
// sandboxprovider.Provider.
//
// A Provider records every call, can delay, hold, fail or crash actors, and
// reports contract violations (overlapping calls on one actor, operations on
// actors in the wrong state) through the testing.TB passed to New.
package fakeprovider

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

// Op identifies a sandboxprovider.Provider method.
type Op string

const (
	OpProvision  Op = "provision"
	OpSnapshot   Op = "snapshot"
	OpRestore    Op = "restore"
	OpDestroy    Op = "destroy"
	OpGetSandbox Op = "get_sandbox"
)

// snapshotBytes is the fixed positive snapshot size reported by Snapshot.
const snapshotBytes int64 = 1 << 20

// Provider is a programmable sandboxprovider.Provider for tests.
type Provider struct {
	t testing.TB

	mu       sync.Mutex
	actors   map[string]sandboxprovider.ActorState
	inFlight map[string]int
	calls    map[Op][]string
	changed  chan struct{}

	delays   map[Op]time.Duration
	holds    map[Op]chan struct{}
	failNext map[Op][]error

	onProvisioned func(runID string)
	onSnapshotted func(actor string)
	onRestored    func(actor string)
	onDestroyed   func(actor string)
}

var _ sandboxprovider.Provider = (*Provider)(nil)

// New returns a Provider that reports contract violations through t.
func New(t testing.TB) *Provider {
	return &Provider{
		t:        t,
		actors:   make(map[string]sandboxprovider.ActorState),
		inFlight: make(map[string]int),
		calls:    make(map[Op][]string),
		changed:  make(chan struct{}),
		delays:   make(map[Op]time.Duration),
		holds:    make(map[Op]chan struct{}),
		failNext: make(map[Op][]error),
	}
}

// Hold makes every call of op that starts after this call block until release
// is called. Calls of other operations are not affected.
func (p *Provider) Hold(op Op) (release func()) {
	ch := make(chan struct{})
	p.mu.Lock()
	p.holds[op] = ch
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if p.holds[op] == ch {
				delete(p.holds, op)
			}
			p.mu.Unlock()
			close(ch)
		})
	}
}

// WaitForCalls waits until at least n calls of op have started, counting all
// calls made since New. It returns ctx.Err() if ctx ends first.
func (p *Provider) WaitForCalls(ctx context.Context, op Op, n int) error {
	for {
		p.mu.Lock()
		started := len(p.calls[op])
		changed := p.changed
		p.mu.Unlock()
		if started >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Calls returns the actor of every started call of op, in start order.
func (p *Provider) Calls(op Op) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls[op]...)
}

// SetDelay makes every call of op wait d before doing its work. The wait is
// canceled when the call's ctx ends.
func (p *Provider) SetDelay(op Op, d time.Duration) {
	p.mu.Lock()
	p.delays[op] = d
	p.mu.Unlock()
}

// FailNext makes the next call of op return err. Failures queue up in call
// order.
func (p *Provider) FailNext(op Op, err error) {
	p.mu.Lock()
	p.failNext[op] = append(p.failNext[op], err)
	p.mu.Unlock()
}

// SetCrashed marks actor as crashed. GetSandbox reports ActorStateCrashed
// afterwards, and Provision and RestoreSnapshot return an error wrapping
// ErrActorCrashed.
func (p *Provider) SetCrashed(actor string) {
	p.mu.Lock()
	p.actors[actor] = sandboxprovider.ActorStateCrashed
	p.mu.Unlock()
}

// OnProvisioned sets a callback invoked with the run id before a successful
// Provision returns.
func (p *Provider) OnProvisioned(fn func(runID string)) {
	p.mu.Lock()
	p.onProvisioned = fn
	p.mu.Unlock()
}

// OnSnapshotted sets a callback invoked with the actor before a successful
// Snapshot returns.
func (p *Provider) OnSnapshotted(fn func(actor string)) {
	p.mu.Lock()
	p.onSnapshotted = fn
	p.mu.Unlock()
}

// OnRestored sets a callback invoked with the actor before a successful
// RestoreSnapshot returns.
func (p *Provider) OnRestored(fn func(actor string)) {
	p.mu.Lock()
	p.onRestored = fn
	p.mu.Unlock()
}

// OnDestroyed sets a callback invoked with the actor before a successful
// Destroy of an existing actor returns.
func (p *Provider) OnDestroyed(fn func(actor string)) {
	p.mu.Lock()
	p.onDestroyed = fn
	p.mu.Unlock()
}

// Provision implements sandboxprovider.Provider.
func (p *Provider) Provision(ctx context.Context, runID string) (sandboxprovider.Timing, error) {
	actor := sandboxprovider.ActorName(runID)
	start := p.begin(OpProvision, actor)
	defer p.end(actor)

	if err := p.wait(ctx, OpProvision); err != nil {
		return sandboxprovider.Timing{}, err
	}
	if err := p.takeFailure(OpProvision); err != nil {
		return sandboxprovider.Timing{}, err
	}

	p.mu.Lock()
	state, exists := p.actors[actor]
	if exists && state == sandboxprovider.ActorStateCrashed {
		p.mu.Unlock()
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: provision %s: %w", actor, sandboxprovider.ErrActorCrashed)
	}
	p.actors[actor] = sandboxprovider.ActorStateRunning
	callback := p.onProvisioned
	p.mu.Unlock()

	timing := sandboxprovider.Timing{Duration: time.Since(start)}
	if callback != nil {
		callback(runID)
	}
	return timing, nil
}

// Snapshot implements sandboxprovider.Provider.
func (p *Provider) Snapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	start := p.begin(OpSnapshot, actor)
	defer p.end(actor)

	if err := p.wait(ctx, OpSnapshot); err != nil {
		return sandboxprovider.Timing{}, err
	}
	if err := p.takeFailure(OpSnapshot); err != nil {
		return sandboxprovider.Timing{}, err
	}

	p.mu.Lock()
	state, exists := p.actors[actor]
	switch {
	case !exists:
		p.mu.Unlock()
		p.t.Errorf("fakeprovider: snapshot on nonexistent actor %q", actor)
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: snapshot %s: %w", actor, sandboxprovider.ErrNotFound)
	case state == sandboxprovider.ActorStateCrashed:
		p.mu.Unlock()
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: snapshot %s: %w", actor, sandboxprovider.ErrActorCrashed)
	case state == sandboxprovider.ActorStateSuspended:
		p.mu.Unlock()
		p.t.Errorf("fakeprovider: snapshot on suspended actor %q", actor)
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: snapshot on suspended actor %q", actor)
	}
	p.actors[actor] = sandboxprovider.ActorStateSuspended
	callback := p.onSnapshotted
	p.mu.Unlock()

	timing := sandboxprovider.Timing{Duration: time.Since(start), SnapshotBytes: snapshotBytes}
	if callback != nil {
		callback(actor)
	}
	return timing, nil
}

// RestoreSnapshot implements sandboxprovider.Provider.
func (p *Provider) RestoreSnapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	start := p.begin(OpRestore, actor)
	defer p.end(actor)

	if err := p.wait(ctx, OpRestore); err != nil {
		return sandboxprovider.Timing{}, err
	}
	if err := p.takeFailure(OpRestore); err != nil {
		return sandboxprovider.Timing{}, err
	}

	p.mu.Lock()
	state, exists := p.actors[actor]
	switch {
	case !exists:
		p.mu.Unlock()
		p.t.Errorf("fakeprovider: restore on nonexistent actor %q", actor)
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: restore %s: %w", actor, sandboxprovider.ErrNotFound)
	case state == sandboxprovider.ActorStateCrashed:
		p.mu.Unlock()
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: restore %s: %w", actor, sandboxprovider.ErrActorCrashed)
	case state == sandboxprovider.ActorStateRunning:
		p.mu.Unlock()
		p.t.Errorf("fakeprovider: restore on running actor %q", actor)
		return sandboxprovider.Timing{}, fmt.Errorf("fakeprovider: restore on running actor %q", actor)
	}
	p.actors[actor] = sandboxprovider.ActorStateRunning
	callback := p.onRestored
	p.mu.Unlock()

	timing := sandboxprovider.Timing{Duration: time.Since(start)}
	if callback != nil {
		callback(actor)
	}
	return timing, nil
}

// Destroy implements sandboxprovider.Provider. Destroying an actor that does
// not exist returns nil.
func (p *Provider) Destroy(ctx context.Context, actor string) error {
	p.begin(OpDestroy, actor)
	defer p.end(actor)

	if err := p.wait(ctx, OpDestroy); err != nil {
		return err
	}
	if err := p.takeFailure(OpDestroy); err != nil {
		return err
	}

	p.mu.Lock()
	delete(p.actors, actor)
	callback := p.onDestroyed
	p.mu.Unlock()

	if callback != nil {
		callback(actor)
	}
	return nil
}

// GetSandbox implements sandboxprovider.Provider.
func (p *Provider) GetSandbox(ctx context.Context, actor string) (sandboxprovider.SandboxStatus, error) {
	p.begin(OpGetSandbox, actor)
	defer p.end(actor)

	if err := p.wait(ctx, OpGetSandbox); err != nil {
		return sandboxprovider.SandboxStatus{}, err
	}
	if err := p.takeFailure(OpGetSandbox); err != nil {
		return sandboxprovider.SandboxStatus{}, err
	}

	p.mu.Lock()
	state, exists := p.actors[actor]
	p.mu.Unlock()
	if !exists {
		return sandboxprovider.SandboxStatus{}, fmt.Errorf("fakeprovider: get %s: %w", actor, sandboxprovider.ErrNotFound)
	}
	return sandboxprovider.SandboxStatus{State: state}, nil
}

// begin records the start of a call and returns its start time. It reports an
// overlapping call on the same actor as a contract violation.
func (p *Provider) begin(op Op, actor string) time.Time {
	p.mu.Lock()
	overlap := p.inFlight[actor] > 0
	p.inFlight[actor]++
	p.calls[op] = append(p.calls[op], actor)
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()

	if overlap {
		p.t.Errorf("fakeprovider: overlapping calls on actor %q", actor)
	}
	return time.Now()
}

// end records the return of a call.
func (p *Provider) end(actor string) {
	p.mu.Lock()
	if n := p.inFlight[actor]; n <= 1 {
		delete(p.inFlight, actor)
	} else {
		p.inFlight[actor] = n - 1
	}
	p.mu.Unlock()
}

// wait applies the delay and hold configured for op. It returns the call's
// ctx error when ctx ends first.
func (p *Provider) wait(ctx context.Context, op Op) error {
	p.mu.Lock()
	delay := p.delays[op]
	hold := p.holds[op]
	p.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if hold != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hold:
		}
	}
	return nil
}

// takeFailure pops the next injected failure for op.
func (p *Provider) takeFailure(op Op) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	queue := p.failNext[op]
	if len(queue) == 0 {
		return nil
	}
	err := queue[0]
	if len(queue) == 1 {
		delete(p.failNext, op)
	} else {
		p.failNext[op] = queue[1:]
	}
	return err
}
