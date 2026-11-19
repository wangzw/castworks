// Package fakewarden is an AgentStream client used as the warden test double.
//
// A Warden follows the warden side of the v0.1.0 protocol described in
// docs/milestone/v0.1.0/agentprotocol.md and docs/milestone/v0.1.0/warden.md:
// every stream opens with Hello, only inbox messages above last_accepted_seq
// are written, every delivery is acknowledged, and turn boundaries are
// reported. Tests drive it through the exported methods to drop frames,
// disconnect, freeze and thaw.
//
// The dial function passed to New transfers ownership of the returned
// connection to the Warden: the connection is closed when the stream that uses
// it ends.
package fakewarden

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// reconnectBase and reconnectMax bound the reconnect backoff used by the
	// fake. They are much shorter than the real warden's because tests run on
	// the order of seconds.
	reconnectBase = 10 * time.Millisecond
	reconnectMax  = 200 * time.Millisecond

	// sendQueue is the number of outbound frames buffered per stream. The
	// sender drains it in order.
	sendQueue = 64

	// sessionID is what the fake reports in turn_started.session_id. The
	// control plane does not inspect it.
	sessionID = "fakewarden-session"

	// waitPoll is how often WaitFor re-evaluates its condition.
	waitPoll = time.Millisecond
)

// DropKind selects which frame the next matching event drops.
type DropKind int

const (
	// DropAccepted writes the delivery and advances the position but does not
	// send Accepted.
	DropAccepted DropKind = iota + 1

	// DropDeliver ignores the whole Deliver frame.
	DropDeliver

	// DropTurnEnded enters the turn boundary and saves last_turn but does not
	// send turn_ended.
	DropTurnEnded
)

// Turn records one turn as the warden reported it.
type Turn struct {
	TurnID      string
	DeliveryID  string // empty for a turn the agent started on its own
	Output      string
	IsError     bool
	AgentExited bool
	Ended       bool
}

// HelloAck is the last HelloAck the warden received.
type HelloAck struct {
	Received bool
	Kind     agentprotocolv1.LLMCredentialKind
	Value    string
}

// Warden is a fake warden process.
type Warden struct {
	t     testing.TB
	runID string

	mu   sync.Mutex
	dial func(context.Context) (*grpc.ClientConn, error)

	phase        agentprotocolv1.Phase
	lastAccepted int64
	lastTurn     *agentprotocolv1.LastTurn
	turnCounter  int
	curTurn      string
	written      []int64
	turns        []Turn
	// deliveries counts every Deliver message the warden processed, by
	// inbox_seq. It is not deduplicated: writing the same seq twice leaves
	// written unchanged but increments deliveries twice.
	deliveries   map[int64]int
	connects     int
	helloAck     HelloAck
	shutdown     bool
	lastClose    codes.Code
	autoEnd      bool
	drop         map[DropKind]int
	frozen       bool
	thawConnects bool
	suppress     bool
	terminal     bool
	started      bool
	stopped      bool

	ctx    context.Context
	cancel context.CancelFunc
	gen    uint64
	cur    *session
	next   time.Duration
	wg     sync.WaitGroup
}

// New returns a Warden for runID. It does not connect until Start is called.
//
// dial is called once per connection attempt. The returned connection is
// owned by the Warden and is closed when that stream ends.
func New(t testing.TB, runID string, dial func(context.Context) (*grpc.ClientConn, error)) *Warden {
	return &Warden{
		t:            t,
		runID:        runID,
		dial:         dial,
		phase:        agentprotocolv1.Phase_PHASE_STARTING,
		autoEnd:      true,
		thawConnects: true,
		drop:         make(map[DropKind]int),
		deliveries:   make(map[int64]int),
	}
}

// SetDial replaces the connection target, for example after the control plane
// restarts.
func (w *Warden) SetDial(dial func(context.Context) (*grpc.ClientConn, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dial = dial
}

// Start connects. When a stream ends it reconnects with a short backoff until
// Stop is called, the stream closes with FailedPrecondition, or a Shutdown
// frame arrives. Start is idempotent.
func (w *Warden) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.stopped {
		return
	}
	w.started = true
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.requestConnectLocked()
}

// Stop closes the current stream and waits for all goroutines to exit. Stop is
// idempotent.
func (w *Warden) Stop() {
	w.mu.Lock()
	if !w.started {
		w.stopped = true
		w.mu.Unlock()
		return
	}
	if w.stopped {
		w.mu.Unlock()
		w.wg.Wait()
		return
	}
	w.stopped = true
	cancel := w.cancel
	cur := w.cur
	w.mu.Unlock()

	cancel()
	if cur != nil {
		cur.cancel()
	}
	w.wg.Wait()
}

// SetAutoEndTurns selects how turns end. When auto is true (the default), a
// Deliver that leaves the warden in a turn immediately reports turn_ended.
// When false, tests call EndTurn.
func (w *Warden) SetAutoEndTurns(auto bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.autoEnd = auto
}

// EndTurn ends the current turn. It does nothing when the warden is not in a
// turn.
func (w *Warden) EndTurn(output string, isError bool) {
	w.mu.Lock()
	s := w.cur
	frames := w.endTurnLocked(output, isError, false)
	w.mu.Unlock()
	enqueueAll(s, frames)
}

// StartSpontaneousTurn starts a turn the agent began on its own. It does
// nothing unless the warden is at a boundary with a live stream.
func (w *Warden) StartSpontaneousTurn() {
	w.mu.Lock()
	if !w.started || w.stopped || w.frozen || w.cur == nil {
		w.mu.Unlock()
		return
	}
	if w.phase != agentprotocolv1.Phase_PHASE_BOUNDARY && w.phase != agentprotocolv1.Phase_PHASE_STARTING {
		w.mu.Unlock()
		return
	}
	s := w.cur
	frames := w.startTurnLocked("")
	w.mu.Unlock()
	enqueueAll(s, frames)
}

// Exit makes the agent exit. In a turn it sends only turn_ended with
// agent_exited; at a boundary it first sends an empty turn_started. Afterwards
// the reported phase is exited.
func (w *Warden) Exit() {
	w.mu.Lock()
	s := w.cur
	var frames []*agentprotocolv1.WardenFrame
	switch w.phase {
	case agentprotocolv1.Phase_PHASE_IN_TURN:
		frames = append(frames, w.endTurnLocked("", false, true)...)
	case agentprotocolv1.Phase_PHASE_BOUNDARY, agentprotocolv1.Phase_PHASE_STARTING:
		frames = append(frames, w.startTurnLocked("")...)
		frames = append(frames, w.endTurnLocked("", false, true)...)
	default:
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	enqueueAll(s, frames)
}

// DropNext drops the next event of the given kind.
func (w *Warden) DropNext(kind DropKind) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drop[kind]++
}

// Disconnect cancels the current stream and reconnects.
func (w *Warden) Disconnect() {
	w.mu.Lock()
	if !w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	w.suppress = false
	old := w.cur
	if old != nil {
		old.retired = true
		old.canceledByUs = true
	}
	w.requestConnectLocked()
	w.mu.Unlock()
	if old != nil {
		old.cancel()
	}
}

// Freeze stops processing and sending without closing the current stream. It
// models a snapshot: the connection stays open but the process does nothing.
func (w *Warden) Freeze() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frozen = true
}

// Thaw opens a new stream, sends Hello on it, and leaves the old stream for
// the control plane to replace. When SetThawConnects(false) is in effect,
// Thaw instead discards the old stream and does not reconnect, which keeps
// the control plane waiting for a Hello.
func (w *Warden) Thaw() {
	w.mu.Lock()
	w.frozen = false
	if !w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	old := w.cur
	if old != nil {
		old.retired = true
	}
	if !w.thawConnects {
		w.suppress = true
		w.gen++
		w.cur = nil
		if old != nil {
			old.canceledByUs = true
		}
		w.mu.Unlock()
		if old != nil {
			old.cancel()
		}
		return
	}
	w.suppress = false
	w.requestConnectLocked()
	w.mu.Unlock()
}

// SetThawConnects controls whether Thaw reconnects. It is true by default.
func (w *Warden) SetThawConnects(connects bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.thawConnects = connects
}

// Written returns the inbox_seq values written to Claude Code, in order.
func (w *Warden) Written() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int64(nil), w.written...)
}

// DeliverCounts returns how many times the warden processed each inbox_seq in
// a Deliver, across all streams. A duplicated Deliver increments the count
// even though the duplicate is not written again.
func (w *Warden) DeliverCounts() map[int64]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return copyDeliverCounts(w.deliveries)
}

// CurrentStreamDeliverCounts returns the per-seq Deliver counts on the current
// stream only. A reconnect starts a fresh count.
func (w *Warden) CurrentStreamDeliverCounts() map[int64]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	return copyDeliverCounts(w.cur.deliveries)
}

func copyDeliverCounts(src map[int64]int) map[int64]int {
	out := make(map[int64]int, len(src))
	for seq, n := range src {
		out[seq] = n
	}
	return out
}

// Turns returns a copy of the turns the warden started, in order.
func (w *Warden) Turns() []Turn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Turn(nil), w.turns...)
}

// Connects returns the number of streams established so far.
func (w *Warden) Connects() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connects
}

// ReceivedHelloAck returns the last HelloAck received.
func (w *Warden) ReceivedHelloAck() HelloAck {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.helloAck
}

// ShutdownReceived reports whether a Shutdown frame arrived.
func (w *Warden) ShutdownReceived() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.shutdown
}

// LastCloseCode returns the status code of the most recent current stream
// close. Replaced streams do not update it.
func (w *Warden) LastCloseCode() codes.Code {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastClose
}

// WaitFor polls cond until it returns true or ctx ends.
func (w *Warden) WaitFor(ctx context.Context, cond func() bool) error {
	if cond() {
		return nil
	}
	ticker := time.NewTicker(waitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if cond() {
				return nil
			}
		}
	}
}

// requestConnectLocked starts a connection attempt. The caller holds w.mu.
func (w *Warden) requestConnectLocked() {
	w.gen++
	gen := w.gen
	w.wg.Add(1)
	go w.connect(gen)
}

func (w *Warden) canConnectLocked(gen uint64) bool {
	return w.started && !w.stopped && !w.frozen && !w.suppress && !w.terminal && !w.shutdown && w.gen == gen
}

func (w *Warden) connect(gen uint64) {
	defer w.wg.Done()

	w.mu.Lock()
	if !w.canConnectLocked(gen) {
		w.mu.Unlock()
		return
	}
	dial := w.dial
	w.mu.Unlock()

	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()

	conn, err := dial(ctx)
	if err != nil {
		w.connectFailed(gen)
		return
	}
	defer func() { _ = conn.Close() }()

	stream, err := agentprotocolv1.NewAgentStreamClient(conn).Connect(ctx)
	if err != nil {
		w.connectFailed(gen)
		return
	}
	if err := stream.Send(w.helloFrame()); err != nil {
		w.connectFailed(gen)
		return
	}

	s := &session{
		w:          w,
		gen:        gen,
		stream:     stream,
		ctx:        ctx,
		cancel:     cancel,
		sendCh:     make(chan *agentprotocolv1.WardenFrame, sendQueue),
		deliveries: make(map[int64]int),
	}

	w.mu.Lock()
	if !w.canConnectLocked(gen) {
		w.mu.Unlock()
		return
	}
	old := w.cur
	w.cur = s
	w.connects++
	w.next = 0
	w.mu.Unlock()

	if old != nil && old != s {
		old.retire()
	}

	err = s.run()
	w.sessionEnded(s, err)
}

func (w *Warden) connectFailed(gen uint64) {
	w.mu.Lock()
	if !w.canConnectLocked(gen) {
		w.mu.Unlock()
		return
	}
	delay := w.nextBackoffLocked()
	w.mu.Unlock()
	w.scheduleConnect(gen, delay)
}

// nextBackoffLocked returns the next retry delay. The caller holds w.mu.
func (w *Warden) nextBackoffLocked() time.Duration {
	if w.next <= 0 {
		w.next = reconnectBase
	} else {
		w.next *= 2
		if w.next > reconnectMax {
			w.next = reconnectMax
		}
	}
	return w.next
}

func (w *Warden) scheduleConnect(gen uint64, delay time.Duration) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-w.ctx.Done():
			return
		case <-timer.C:
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.canConnectLocked(gen) {
			return
		}
		w.requestConnectLocked()
	}()
}

func (w *Warden) sessionEnded(s *session, err error) {
	code := closeCode(err)

	w.mu.Lock()
	if s.gen != w.gen {
		w.mu.Unlock()
		return
	}
	if w.cur == s {
		w.cur = nil
	}
	w.lastClose = code
	if code == codes.FailedPrecondition {
		w.terminal = true
	}
	if !w.started || w.stopped || w.frozen || w.suppress || w.terminal || w.shutdown {
		w.mu.Unlock()
		return
	}
	if s.canceledByUs {
		w.next = 0
		w.requestConnectLocked()
		w.mu.Unlock()
		return
	}
	delay := w.nextBackoffLocked()
	w.mu.Unlock()
	w.scheduleConnect(s.gen, delay)
}

func (w *Warden) helloFrame() *agentprotocolv1.WardenFrame {
	w.mu.Lock()
	defer w.mu.Unlock()

	hello := &agentprotocolv1.Hello{
		AgentState:      &agentprotocolv1.AgentState{Phase: w.phase},
		LastAcceptedSeq: w.lastAccepted,
	}
	if w.phase == agentprotocolv1.Phase_PHASE_IN_TURN {
		hello.AgentState.TurnId = w.curTurn
	}
	if w.phase == agentprotocolv1.Phase_PHASE_BOUNDARY && w.lastTurn != nil {
		hello.LastTurn = &agentprotocolv1.LastTurn{
			TurnId:  w.lastTurn.TurnId,
			Output:  w.lastTurn.Output,
			IsError: w.lastTurn.IsError,
		}
	}
	return &agentprotocolv1.WardenFrame{
		RunId: w.runID,
		Body:  &agentprotocolv1.WardenFrame_Hello{Hello: hello},
	}
}

func (w *Warden) handleFrame(s *session, f *agentprotocolv1.ControlFrame) {
	if err := agentprotocol.ValidateControlFrame(f); err != nil {
		w.t.Errorf("fakewarden: invalid control frame: %v", err)
		return
	}

	var (
		frames    []*agentprotocolv1.WardenFrame
		violation string
	)
	w.mu.Lock()
	if s != w.cur || s.retired || w.frozen {
		w.mu.Unlock()
		return
	}
	if !s.helloAckSeen {
		if _, ok := f.GetBody().(*agentprotocolv1.ControlFrame_HelloAck); !ok {
			violation = fmt.Sprintf("fakewarden: %T arrived on a new stream before HelloAck", f.GetBody())
		}
	}
	switch body := f.GetBody().(type) {
	case *agentprotocolv1.ControlFrame_HelloAck:
		s.helloAckSeen = true
		w.helloAck.Received = true
		if c := body.HelloAck.GetLlmCredential(); c != nil {
			w.helloAck.Kind = c.GetKind()
			w.helloAck.Value = c.GetValue()
		}
	case *agentprotocolv1.ControlFrame_Shutdown:
		w.shutdown = true
	case *agentprotocolv1.ControlFrame_Deliver:
		// Count every message the warden processed before the deduplication
		// in deliverLocked, so a duplicate Deliver is visible even though it
		// does not produce a second written entry.
		for _, m := range body.Deliver.GetBatch() {
			w.deliveries[m.GetInboxSeq()]++
			s.deliveries[m.GetInboxSeq()]++
		}
		frames = w.deliverLocked(body.Deliver)
	}
	w.mu.Unlock()

	if violation != "" {
		w.t.Errorf("%s", violation)
	}
	enqueueAll(s, frames)
}

func (w *Warden) deliverLocked(d *agentprotocolv1.Deliver) []*agentprotocolv1.WardenFrame {
	if w.drop[DropDeliver] > 0 {
		w.drop[DropDeliver]--
		return nil
	}

	fresh := false
	for _, m := range d.GetBatch() {
		if m.GetInboxSeq() <= w.lastAccepted {
			continue
		}
		fresh = true
		w.written = append(w.written, m.GetInboxSeq())
	}
	if d.GetThroughSeq() > w.lastAccepted {
		w.lastAccepted = d.GetThroughSeq()
	}

	var frames []*agentprotocolv1.WardenFrame
	if w.drop[DropAccepted] > 0 {
		w.drop[DropAccepted]--
	} else {
		frames = append(frames, acceptedFrame(w.runID, d.GetDeliveryId(), d.GetThroughSeq()))
	}

	if fresh && (w.phase == agentprotocolv1.Phase_PHASE_BOUNDARY ||
		w.phase == agentprotocolv1.Phase_PHASE_STARTING) {
		frames = append(frames, w.startTurnLocked(d.GetDeliveryId())...)
	}

	if fresh && w.autoEnd && w.phase == agentprotocolv1.Phase_PHASE_IN_TURN {
		frames = append(frames, w.endTurnLocked("", false, false)...)
	}
	return frames
}

func (w *Warden) startTurnLocked(deliveryID string) []*agentprotocolv1.WardenFrame {
	w.turnCounter++
	turnID := fmt.Sprintf("turn-%d", w.turnCounter)
	w.curTurn = turnID
	w.phase = agentprotocolv1.Phase_PHASE_IN_TURN
	w.turns = append(w.turns, Turn{TurnID: turnID, DeliveryID: deliveryID})
	return []*agentprotocolv1.WardenFrame{turnStartedFrame(w.runID, turnID, deliveryID)}
}

func (w *Warden) endTurnLocked(output string, isError, agentExited bool) []*agentprotocolv1.WardenFrame {
	if w.curTurn == "" || w.phase != agentprotocolv1.Phase_PHASE_IN_TURN {
		return nil
	}
	turnID := w.curTurn
	if n := len(w.turns); n > 0 && w.turns[n-1].TurnID == turnID {
		w.turns[n-1].Output = output
		w.turns[n-1].IsError = isError
		w.turns[n-1].AgentExited = agentExited
		w.turns[n-1].Ended = true
	}
	w.lastTurn = &agentprotocolv1.LastTurn{TurnId: turnID, Output: output, IsError: isError}
	w.curTurn = ""
	if agentExited {
		w.phase = agentprotocolv1.Phase_PHASE_EXITED
	} else {
		w.phase = agentprotocolv1.Phase_PHASE_BOUNDARY
	}
	if w.drop[DropTurnEnded] > 0 {
		w.drop[DropTurnEnded]--
		return nil
	}
	return []*agentprotocolv1.WardenFrame{turnEndedFrame(w.runID, turnID, output, isError, agentExited)}
}

// session is one AgentStream connection.
type session struct {
	w      *Warden
	gen    uint64
	stream agentprotocolv1.AgentStream_ConnectClient
	ctx    context.Context
	cancel context.CancelFunc
	sendCh chan *agentprotocolv1.WardenFrame

	// The following fields are guarded by w.mu.
	helloAckSeen bool
	retired      bool
	canceledByUs bool
	deliveries   map[int64]int
}

func (s *session) retire() {
	s.w.mu.Lock()
	s.retired = true
	s.w.mu.Unlock()
}

func (s *session) run() error {
	var senders sync.WaitGroup
	senders.Add(1)
	go func() {
		defer senders.Done()
		s.sendLoop()
	}()
	err := s.recvLoop()
	s.cancel()
	senders.Wait()
	return err
}

func (s *session) recvLoop() error {
	for {
		f, err := s.stream.Recv()
		if err != nil {
			return err
		}
		s.w.handleFrame(s, f)
		s.w.mu.Lock()
		stop := s.w.shutdown || s.w.stopped
		s.w.mu.Unlock()
		if stop {
			return nil
		}
	}
}

func (s *session) sendLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case f := <-s.sendCh:
			s.w.mu.Lock()
			drop := s.retired || s.w.frozen || s.w.cur != s || s.w.stopped
			s.w.mu.Unlock()
			if drop {
				continue
			}
			if err := s.stream.Send(f); err != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *session) enqueue(f *agentprotocolv1.WardenFrame) {
	s.w.mu.Lock()
	ok := s.w.cur == s && !s.retired && !s.w.frozen && !s.w.stopped
	s.w.mu.Unlock()
	if !ok {
		return
	}
	select {
	case s.sendCh <- f:
	case <-s.ctx.Done():
	}
}

func enqueueAll(s *session, frames []*agentprotocolv1.WardenFrame) {
	if s == nil {
		return
	}
	for _, f := range frames {
		s.enqueue(f)
	}
}

func turnStartedFrame(runID, turnID, deliveryID string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: turnID,
				Kind: &agentprotocolv1.AgentEvent_TurnStarted{
					TurnStarted: &agentprotocolv1.TurnStarted{
						DeliveryId: deliveryID,
						SessionId:  sessionID,
					},
				},
			},
		},
	}
}

func turnEndedFrame(runID, turnID, output string, isError, agentExited bool) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: turnID,
				Kind: &agentprotocolv1.AgentEvent_TurnEnded{
					TurnEnded: &agentprotocolv1.TurnEnded{
						Output:      output,
						IsError:     isError,
						AgentExited: agentExited,
					},
				},
			},
		},
	}
}

func acceptedFrame(runID, deliveryID string, throughSeq int64) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Accepted{
			Accepted: &agentprotocolv1.Accepted{
				DeliveryId: deliveryID,
				ThroughSeq: throughSeq,
			},
		},
	}
}

func closeCode(err error) codes.Code {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return codes.OK
	case errors.Is(err, context.Canceled):
		return codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded
	}
	return status.Code(err)
}
