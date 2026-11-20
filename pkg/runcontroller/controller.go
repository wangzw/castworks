package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultIdleGrace    = 30 * time.Second
	defaultScanPeriod   = 5 * time.Second
	defaultHelloTimeout = 60 * time.Second

	// runIDLength is the number of characters in a run_id and a delivery_id.
	runIDLength = 12

	// runIDAlphabet is the lowercase base32 alphabet a run_id is drawn from.
	runIDAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

	// maxDeliverBytes bounds the total text of one Deliver batch. The rest is
	// delivered at the next turn boundary, keeping the frame under the gRPC
	// default 4 MiB message limit.
	maxDeliverBytes = 1 << 20
)

// Controller implements RunService, the agentEvents event entry points and
// scanTarget. It assembles the store, the stream registry and the provider
// call queue.
type Controller struct {
	cfg      Config
	store    *store
	provider sandboxprovider.Provider
	clock    Clock
	streams  *streamRegistry
	calls    *providerQueue

	hookMu      sync.Mutex
	afterLock   func(op, runID string)
	afterCommit func(op, runID string)

	// runMu guards runCtx, the context of the running scan loop. Completion and
	// stream-close writes retry until they succeed or this context ends.
	runMu  sync.Mutex
	runCtx context.Context

	// streamCloseAttempts overrides the stream-close retry bound. Tests set it
	// through export_test.go; zero means streamCloseRetryAttempts. A stream
	// handler may read it concurrently, so it is atomic.
	streamCloseAttempts atomic.Int32
}

// streamCloseRetryAttempts bounds the retries of a stream-close write. The
// stream handler returns as soon as onStreamClosed returns, so this write has
// no live context to observe beyond Run's; when Run is not started there is
// none at all, and an unbounded retry would never exit.
const streamCloseRetryAttempts = 5

var (
	_ RunService  = (*Controller)(nil)
	_ scanTarget  = (*Controller)(nil)
	_ agentEvents = (*Controller)(nil)
	_ scanSource  = (*store)(nil)
)

// New builds a Controller. It does not run the database migrations; call
// Migrate separately. New panics when the configured credential is incomplete.
func New(cfg Config, pool *pgxpool.Pool, provider sandboxprovider.Provider, clock Clock) *Controller {
	if cfg.IdleGrace == 0 {
		cfg.IdleGrace = defaultIdleGrace
	}
	if cfg.ScanPeriod == 0 {
		cfg.ScanPeriod = defaultScanPeriod
	}
	if cfg.HelloTimeout == 0 {
		cfg.HelloTimeout = defaultHelloTimeout
	}
	switch cfg.Credential.Kind {
	case LLMCredentialAPIKey, LLMCredentialOAuthToken:
	default:
		panic("runcontroller: credential kind is not api_key or oauth_token")
	}
	if cfg.Credential.Value == "" {
		panic("runcontroller: credential value is empty")
	}

	return &Controller{
		cfg:      cfg,
		store:    newStore(pool),
		provider: provider,
		clock:    clock,
		streams:  newStreamRegistry(),
		calls:    newProviderQueue(),
	}
}

// RegisterGRPC registers the AgentStream service on s.
func (c *Controller) RegisterGRPC(s *grpc.Server) {
	agentprotocolv1.RegisterAgentStreamServer(s, newAgentStreamServer(c, c.streams))
}

// Run runs the suspension scan and the T_hello checks until ctx ends, then
// cancels and waits for in-flight provider calls before returning nil.
func (c *Controller) Run(ctx context.Context) error {
	c.runMu.Lock()
	c.runCtx = ctx
	c.runMu.Unlock()
	defer func() {
		c.runMu.Lock()
		c.runCtx = nil
		c.runMu.Unlock()
	}()

	_ = runScanLoop(ctx, c.cfg, c.clock, c.store, c)
	c.calls.shutdown()
	return nil
}

// runDone is closed when Run's context ends, or nil when Run is not running.
// Select statements on a nil channel block forever, so retries continue until
// they succeed when Run has not been started.
func (c *Controller) runDone() <-chan struct{} {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.runCtx == nil {
		return nil
	}
	return c.runCtx.Done()
}

// setAfterLock installs the test hook invoked after each rule takes the run
// row lock and before it writes. It is only exported to tests by
// export_test.go.
func (c *Controller) setAfterLock(fn func(op, runID string)) {
	c.hookMu.Lock()
	c.afterLock = fn
	c.hookMu.Unlock()
}

func (c *Controller) callAfterLock(op, runID string) {
	c.hookMu.Lock()
	fn := c.afterLock
	c.hookMu.Unlock()
	if fn != nil {
		fn(op, runID)
	}
}

// setAfterCommit installs the test hook invoked after a rule transaction
// commits and before its reserved frames are released. It is only exported to
// tests by export_test.go.
func (c *Controller) setAfterCommit(fn func(op, runID string)) {
	c.hookMu.Lock()
	c.afterCommit = fn
	c.hookMu.Unlock()
}

func (c *Controller) callAfterCommit(op, runID string) {
	c.hookMu.Lock()
	fn := c.afterCommit
	c.hookMu.Unlock()
	if fn != nil {
		fn(op, runID)
	}
}

// trySuspend implements scanTarget.
func (c *Controller) trySuspend(ctx context.Context, runID string) error {
	idleBefore := c.clock.Now().Add(-c.cfg.IdleGrace)
	_, err := c.applyEvent(ctx, runID, evTrySuspend{idleBefore: idleBefore}, applyOpts{})
	return err
}

// expireHello implements scanTarget.
func (c *Controller) expireHello(ctx context.Context, runID string) error {
	returnedBefore := c.clock.Now().Add(-c.cfg.HelloTimeout)
	_, err := c.applyEvent(ctx, runID, evHelloExpire{returnedBefore: returnedBefore}, applyOpts{})
	return err
}

// applyOpts carries the stream context of an event application.
type applyOpts struct {
	// registerConn is the conn a registerStream effect registers. It is set
	// only by onHello.
	registerConn *conn
	// requireCurrent, when set, makes the whole application a no-op unless
	// this conn is still the registered stream when the row lock is taken.
	requireCurrent *conn
}

// pendingProvider is a provider effect reserved during the transaction and
// started after it commits.
type pendingProvider struct {
	effect machineEffect
	slot   *providerSlot
}

// pendingClose is a stream close performed only after the transaction commits
// and the reserved frames have been released. Closing inside the transaction
// would make the send goroutine drop a Shutdown that is still unreleased.
type pendingClose struct {
	conn *conn
	st   *status.Status
}

// applyEvent runs one rule: it locks the run row, computes the transition and
// writes the resulting state and effects in a single transaction. Stream
// frames are reserved in order while the row lock is held and released after
// the commit (or canceled on rollback), so a frame never leaves before its
// transaction commits. Provider effects reserve a queue slot while the lock is
// held and start after the commit.
//
// If the commit itself fails, the outcome is uncertain and is decided by
// PostgreSQL's transaction status (pg_xact_status), not by re-reading the run
// or comparing row versions.
func (c *Controller) applyEvent(
	ctx context.Context,
	runID string,
	ev machineEvent,
	opts applyOpts,
) ([]machineEffect, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := c.clock.Now()

	var (
		effects []machineEffect
		pending []pendingProvider
		frames  []*frameSlot
		closes  []pendingClose
	)

	// Whatever ends this application, a panic must not leave reserved frames
	// blocked or reserved provider slots dangling: undo both before the panic
	// leaves the rule. release/cancel and start/abandon are each idempotent,
	// so the normal paths may resolve reservations first.
	defer func() {
		for _, s := range frames {
			s.cancel()
		}
		for _, p := range pending {
			p.slot.abandon()
		}
	}()

	err := c.store.inTx(ctx, func(tx *storeTx) error {
		row, err := tx.lockRun(runID)
		if err != nil {
			return err
		}
		if opts.requireCurrent != nil && c.streams.current(runID) != opts.requireCurrent {
			// A newer stream has replaced this one. Frames and the close of the
			// old stream must not change the run.
			return nil
		}

		// The hook runs after the row lock is taken and before anything is
		// written. Tests use it to force the two orders of an interleaving.
		c.callAfterLock(eventName(ev), runID)

		env, err := c.machineEnv(tx, runID, now)
		if err != nil {
			return err
		}
		result, err := transition(stateFromRow(row), ev, env)
		if err != nil {
			return err
		}
		effects = result.Effects

		for _, effect := range result.Effects {
			switch e := effect.(type) {
			case insertInbox:
				if err := tx.insertInbox(runID, e.seq, e.text, now); err != nil {
					return err
				}
			case turnStarted:
				if err := tx.upsertTurnStarted(runID, e.turnID, e.deliveryID, e.at); err != nil {
					return err
				}
			case turnEnded:
				if err := tx.upsertTurnEnded(runID, e.turnID, e.at, e.output, e.isError); err != nil {
					return err
				}
			case openSuspension:
				if err := tx.openSuspension(runID, e.at); err != nil {
					return err
				}
			case updateSuspension:
				if err := tx.updateLatestSuspension(runID, suspensionUpdate{
					SnapshotMS:        e.snapshotMS,
					SnapshotBytes:     e.snapshotBytes,
					ResumeRequestedAt: e.resumeRequestedAt,
					RestoreMS:         e.restoreMS,
					RestoreReturnedAt: e.restoreReturnedAt,
					HelloAt:           e.helloAt,
					HelloMS:           e.helloMS,
					Error:             e.err,
				}); err != nil {
					return err
				}
			case registerStream:
				if opts.registerConn == nil {
					return errors.New("runcontroller: registerStream without a conn")
				}
				if old := c.streams.replace(runID, opts.registerConn); old != nil && old != opts.registerConn {
					old.closeWith(status.New(codes.Aborted, "runcontroller: stream replaced"))
				}
			case unregisterStream:
				if opts.requireCurrent != nil {
					c.streams.removeIf(runID, opts.requireCurrent)
				}
			case sendHelloAck:
				target := c.streams.current(runID)
				if target == nil {
					return errors.New("runcontroller: hello ack without a registered stream")
				}
				slot, err := target.reserve(c.helloAckFrame(runID), 0)
				if err != nil {
					return err
				}
				frames = append(frames, slot)
			case deliver:
				target := c.streams.current(runID)
				if target == nil {
					return errors.New("runcontroller: deliver without a registered stream")
				}
				messages, err := tx.inboxRange(runID, e.from, e.through, maxDeliverBytes)
				if err != nil {
					return err
				}
				if len(messages) == 0 {
					return fmt.Errorf("runcontroller: deliver %s [%d, %d] selected no messages", runID, e.from, e.through)
				}
				frame, err := c.deliverFrame(runID, messages)
				if err != nil {
					return err
				}
				// The reserved position must be the last inbox_seq actually in
				// the frame, not the rule's full through: inboxRange may cut
				// the batch at maxDeliverBytes, and the warden echoes the
				// frame's through_seq in Accepted.
				slot, err := target.reserve(frame, messages[len(messages)-1].Seq)
				if err != nil {
					if !errors.Is(err, errStreamClosed) {
						return err
					}
					// The stream closed after the rule saw it as connected. A
					// closed stream is a disconnected run, not a failed write:
					// keep the inbox rows, drop the registration and put the
					// run back at its boundary. The next Hello redelivers.
					c.streams.removeIf(runID, target)
					if result.State.State == StateActive && result.State.Substate == SubstateDelivering {
						result.State.Substate = SubstateBoundary
						now := env.Now
						result.State.IdleSince = &now
					}
					continue
				}
				frames = append(frames, slot)
			case sendShutdown:
				target := c.streams.current(runID)
				if target != nil {
					slot, err := target.reserve(shutdownFrame(runID), 0)
					switch {
					case errors.Is(err, errStreamClosed):
						// The stream is closed but still registered. End ignores
						// a Shutdown it cannot send and commits normally.
					case err != nil:
						return err
					default:
						frames = append(frames, slot)
					}
				}
			case closeStream:
				target := c.streams.current(runID)
				if opts.registerConn != nil {
					// Close the stream that sent this Hello, not a stale
					// registration: on phase exited the new stream is not
					// registered, and the old one must not be closed in its
					// place.
					target = opts.registerConn
				}
				if target != nil {
					// Defer the close until after the commit and after the
					// frames reserved before it are released. End reserves a
					// Shutdown and then closes the stream; closing now would
					// make pump drop the still-unreleased Shutdown.
					closes = append(closes, pendingClose{conn: target, st: status.New(e.code, "")})
				}
			case callProvision, callSnapshot, callRestore, callDestroy:
				pending = append(pending, pendingProvider{
					effect: effect,
					slot:   c.calls.reserve(runID),
				})
			default:
				return fmt.Errorf("runcontroller: unhandled effect %T", effect)
			}
		}

		return tx.updateRun(rowFromState(row, result.State))
	})

	committed := err == nil
	if err != nil {
		var ce *commitError
		if errors.As(err, &ce) {
			committed = c.resolveCommit(runID, ce, frames, pending)
		}
	}
	if committed {
		// The hook runs after the commit and before the reserved frames are
		// released. Tests use it to prove a frame is never enqueued after the
		// commit has become visible.
		c.callAfterCommit(eventName(ev), runID)
		for _, s := range frames {
			s.release()
		}
		// Close after the frames reserved before the close are released, so a
		// stream that is being ended still receives its Shutdown first.
		for _, cl := range closes {
			cl.conn.closeWith(cl.st)
		}
		for _, p := range pending {
			c.startProviderCall(runID, p)
		}
		return effects, nil
	}

	for _, s := range frames {
		s.cancel()
	}
	for _, p := range pending {
		p.slot.abandon()
	}
	return nil, err
}

// resolveCommit decides whether the transaction whose Commit returned ce
// committed, using PostgreSQL's transaction status.
//
// When the outcome cannot be determined after bounded retries, it undoes the
// rule's reservations: reserved frames are canceled, the run's current stream
// is closed so the warden reconnects and reconciles with Hello, and reserved
// provider calls are abandoned. It logs the failure and reports false.
func (c *Controller) resolveCommit(
	runID string,
	ce *commitError,
	frames []*frameSlot,
	pending []pendingProvider,
) bool {
	switch c.store.commitOutcome(ce) {
	case commitCommitted:
		return true
	case commitAborted:
		return false
	}

	// Close before canceling the reserved frames. If the transaction did
	// commit, canceling a position makes pump skip it and send the frames
	// queued behind it; closing first makes pump drop the still-unreleased
	// position and exit, so the warden never sees a sequence gap and
	// reconciles through its next Hello instead.
	if conn := c.streams.current(runID); conn != nil {
		conn.closeWith(status.New(codes.Unavailable, "runcontroller: commit outcome unknown"))
	}
	for _, s := range frames {
		s.cancel()
	}
	// Known limitation: when the transaction actually committed but the
	// outcome cannot be determined, the provider call it reserved is
	// abandoned and the run can stall in provisioning, resuming or
	// suspending. The scan does not start such a call, and a reconnect
	// through Hello does not either. This requires the database to stay
	// unreachable for the whole ~10 s outcome window at commit time.
	for _, p := range pending {
		p.slot.abandon()
	}
	slog.Error("runcontroller: commit outcome unknown after bounded retries",
		"run_id", runID, "error", ce.err)
	return false
}

// machineEnv builds the non-row part of the rule environment inside the
// transaction.
func (c *Controller) machineEnv(tx *storeTx, runID string, now time.Time) (machineEnv, error) {
	env := machineEnv{
		Now:             now,
		StreamConnected: c.streams.current(runID) != nil,
	}
	susp, ok, err := tx.latestSuspension(runID)
	if err != nil {
		return machineEnv{}, err
	}
	if ok {
		env.Suspension = &machineSuspension{
			RestoreReturnedAt: susp.RestoreReturnedAt,
			HelloAt:           susp.HelloAt,
		}
	}
	return env, nil
}

// startProviderCall starts the provider call for a reserved effect.
func (c *Controller) startProviderCall(runID string, p pendingProvider) {
	actor := sandboxprovider.ActorName(runID)
	switch p.effect.(type) {
	case callProvision:
		p.slot.start(providerCallJob(opProvision, runID, func(ctx context.Context) error {
			_, err := c.provider.Provision(ctx, runID)
			// The event carries the moment the call returned, not the moment a
			// retried write succeeds: T_hello starts at the return.
			returnedAt := c.clock.Now()
			c.complete(ctx, runID, evProvisionDone{returnedAt: returnedAt, err: err})
			return err
		}))
	case callSnapshot:
		p.slot.start(providerCallJob(opSnapshot, runID, func(ctx context.Context) error {
			timing, err := c.provider.Snapshot(ctx, actor)
			crashed := false
			if err != nil {
				crashed = c.actorCrashed(ctx, runID, actor, err)
			}
			c.complete(ctx, runID, evSnapshotDone{timing: timing, err: err, crashed: crashed})
			return err
		}))
	case callRestore:
		p.slot.start(providerCallJob(opRestore, runID, func(ctx context.Context) error {
			timing, err := c.provider.RestoreSnapshot(ctx, actor)
			// As with Provision, carry the actual return moment so a delayed
			// completion write does not postpone T_hello.
			returnedAt := c.clock.Now()
			c.complete(ctx, runID, evRestoreDone{timing: timing, err: err, returnedAt: returnedAt})
			return err
		}))
	case callDestroy:
		p.slot.start(providerCallJob(opDestroy, runID, func(ctx context.Context) error {
			err := c.provider.Destroy(ctx, actor)
			if err != nil {
				slog.Error("runcontroller: destroy failed", "run_id", runID, "error", err)
			}
			return err
		}))
	}
}

// actorCrashed decides whether a failed Snapshot means the actor crashed. It
// calls GetSandbox in the same provider job, as the decision requires. A
// GetSandbox error or an unknown state counts as not crashed.
func (c *Controller) actorCrashed(ctx context.Context, runID, actor string, snapshotErr error) bool {
	if errors.Is(snapshotErr, sandboxprovider.ErrActorCrashed) {
		return true
	}

	var (
		sbStatus sandboxprovider.SandboxStatus
		sbErr    error
	)
	providerCallJob(opGetSandbox, runID, func(ctx context.Context) error {
		sbStatus, sbErr = c.provider.GetSandbox(ctx, actor)
		return sbErr
	})(ctx)
	if sbErr != nil {
		return false
	}
	return sbStatus.State == sandboxprovider.ActorStateCrashed
}

// Retry backoff for completion and stream-close writes.
const (
	writeRetryInitial = 1 * time.Second
	writeRetryMax     = 10 * time.Second
)

// applyEventRetry applies ev until it succeeds, the Run context ends or ctx is
// canceled, whichever comes first. Each failure is logged at Warn. ErrNotFound
// is permanent and stops the retry. maxAttempts bounds the number of
// applications; zero means no bound (used by completion writes).
func (c *Controller) applyEventRetry(
	ctx context.Context,
	runID string,
	ev machineEvent,
	opts applyOpts,
	maxAttempts int,
) {
	delay := writeRetryInitial
	for attempt := 1; ; attempt++ {
		_, err := c.applyEvent(ctx, runID, ev, opts)
		if err == nil || errors.Is(err, ErrNotFound) {
			return
		}
		if ctx.Err() != nil {
			// The provider call or stream is shutting down; nothing to retry.
			return
		}
		if maxAttempts > 0 && attempt >= maxAttempts {
			slog.Warn("runcontroller: event write gave up after bounded retries",
				"run_id", runID, "event", eventName(ev), "attempts", attempt, "error", err)
			return
		}
		slog.Warn("runcontroller: event write failed, retrying",
			"run_id", runID, "event", eventName(ev), "error", err, "retry_in", delay)
		select {
		case <-time.After(delay):
		case <-c.runDone():
			return
		case <-ctx.Done():
			return
		}
		if delay < writeRetryMax {
			delay *= 2
			if delay > writeRetryMax {
				delay = writeRetryMax
			}
		}
	}
}

// streamCloseLimit is the retry bound for a stream-close write.
func (c *Controller) streamCloseLimit() int {
	if n := c.streamCloseAttempts.Load(); n > 0 {
		return int(n)
	}
	return streamCloseRetryAttempts
}

// complete applies a provider completion event unless the provider task's ctx
// was canceled (Run is shutting down), in which case the run stays in its
// transitional state. The completion write uses its own context so that the
// caller's request context never affects it, and retries until it succeeds or
// Run's context ends.
func (c *Controller) complete(taskCtx context.Context, runID string, ev machineEvent) {
	if taskCtx.Err() != nil {
		return
	}
	c.applyEventRetry(taskCtx, runID, ev, applyOpts{}, 0)
}

// helloAckFrame builds the HelloAck carrying the configured credential.
func (c *Controller) helloAckFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_HelloAck{
			HelloAck: &agentprotocolv1.HelloAck{
				LlmCredential: &agentprotocolv1.LLMCredential{
					Kind:  credentialKindProto(c.cfg.Credential.Kind),
					Value: c.cfg.Credential.Value,
				},
			},
		},
	}
}

// deliverFrame builds a Deliver frame from the batch selected out of the
// inbox.
func (c *Controller) deliverFrame(runID string, messages []inboxMessage) (*agentprotocolv1.ControlFrame, error) {
	deliveryID, err := randomID(runIDLength)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: generate delivery id: %w", err)
	}
	batch := make([]*agentprotocolv1.InboxMessage, len(messages))
	for i, m := range messages {
		batch[i] = &agentprotocolv1.InboxMessage{
			InboxSeq: m.Seq,
			Content:  &agentprotocolv1.InboxMessage_Text{Text: m.Text},
		}
	}
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_Deliver{
			Deliver: &agentprotocolv1.Deliver{
				DeliveryId: deliveryID,
				Batch:      batch,
				ThroughSeq: messages[len(messages)-1].Seq,
			},
		},
	}, nil
}

func shutdownFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_Shutdown{
			Shutdown: &agentprotocolv1.Shutdown{Reason: "run ended"},
		},
	}
}

func credentialKindProto(kind LLMCredentialKind) agentprotocolv1.LLMCredentialKind {
	switch kind {
	case LLMCredentialAPIKey:
		return agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY
	case LLMCredentialOAuthToken:
		return agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_OAUTH_TOKEN
	default:
		return agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_UNSPECIFIED
	}
}

func stateFromRow(r runRow) machineState {
	return machineState{
		State:               r.State,
		Substate:            r.Substate,
		NextInboxSeq:        r.NextInboxSeq,
		DeliveredThrough:    r.DeliveredThrough,
		IdleSince:           r.IdleSince,
		ProvisionReturnedAt: r.ProvisionReturnedAt,
		FailureReason:       r.FailureReason,
		EndedAt:             r.EndedAt,
	}
}

func rowFromState(r runRow, s machineState) runRow {
	r.State = s.State
	r.Substate = s.Substate
	r.NextInboxSeq = s.NextInboxSeq
	r.DeliveredThrough = s.DeliveredThrough
	r.IdleSince = s.IdleSince
	r.ProvisionReturnedAt = s.ProvisionReturnedAt
	r.FailureReason = s.FailureReason
	r.EndedAt = s.EndedAt
	return r
}

// eventName is the name of an event as shown to the after-lock hook.
func eventName(ev machineEvent) string {
	switch ev.(type) {
	case evAppend:
		return "append"
	case evProvisionDone:
		return "provision_done"
	case evHello:
		return "hello"
	case evAccepted:
		return "accepted"
	case evTurnStarted:
		return "turn_started"
	case evTurnEnded:
		return "turn_ended"
	case evStreamClosed:
		return "stream_closed"
	case evEnd:
		return "end"
	case evTrySuspend:
		return "try_suspend"
	case evSnapshotDone:
		return "snapshot_done"
	case evRestoreDone:
		return "restore_done"
	case evHelloExpire:
		return "hello_expire"
	default:
		return fmt.Sprintf("%T", ev)
	}
}
