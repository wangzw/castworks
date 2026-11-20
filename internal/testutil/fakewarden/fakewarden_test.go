package fakewarden_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const testRunID = "run-123"

// testServer is a minimal AgentStream server. It records warden frames and
// lets the test push control frames or close a stream with a chosen status.
type testServer struct {
	agentprotocolv1.UnimplementedAgentStreamServer

	t   *testing.T
	lis *bufconn.Listener
	srv *grpc.Server

	mu      sync.Mutex
	streams []*testStream
}

type testStream struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	sendCh  chan *agentprotocolv1.ControlFrame
	closeCh chan codes.Code

	mu     sync.Mutex
	frames []*agentprotocolv1.WardenFrame
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	s := &testServer{t: t, lis: lis, srv: srv}
	agentprotocolv1.RegisterAgentStreamServer(srv, s)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return s
}

func (s *testServer) Connect(srvStream agentprotocolv1.AgentStream_ConnectServer) error {
	ctx, cancel := context.WithCancel(srvStream.Context())
	defer cancel()
	ss := &testStream{
		t:       s.t,
		ctx:     ctx,
		cancel:  cancel,
		sendCh:  make(chan *agentprotocolv1.ControlFrame, 64),
		closeCh: make(chan codes.Code, 1),
	}
	s.mu.Lock()
	s.streams = append(s.streams, ss)
	s.mu.Unlock()

	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := srvStream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			ss.mu.Lock()
			ss.frames = append(ss.frames, f)
			ss.mu.Unlock()
		}
	}()

	for {
		select {
		case f := <-ss.sendCh:
			if err := srvStream.Send(f); err != nil {
				return err
			}
		case code := <-ss.closeCh:
			if code == codes.OK {
				return nil
			}
			return status.Error(code, "test closes the stream")
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

func (s *testServer) dial(ctx context.Context) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return s.lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

func (s *testServer) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *testServer) streamAt(i int) *testStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[i]
}

func (s *testServer) waitStream(t *testing.T, ctx context.Context, n int) *testStream {
	t.Helper()
	if err := waitFor(ctx, func() bool { return s.streamCount() >= n }); err != nil {
		t.Fatalf("waiting for stream %d: %v", n, err)
	}
	return s.streamAt(n - 1)
}

func (ss *testStream) send(f *agentprotocolv1.ControlFrame) {
	select {
	case ss.sendCh <- f:
	case <-ss.ctx.Done():
	}
}

func (ss *testStream) closeWith(code codes.Code) {
	select {
	case ss.closeCh <- code:
	case <-ss.ctx.Done():
	}
}

func (ss *testStream) snapshot() []*agentprotocolv1.WardenFrame {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return append([]*agentprotocolv1.WardenFrame(nil), ss.frames...)
}

func (ss *testStream) waitHello(t *testing.T, ctx context.Context) *agentprotocolv1.Hello {
	t.Helper()
	var hello *agentprotocolv1.Hello
	err := waitFor(ctx, func() bool {
		for _, f := range ss.snapshot() {
			if h := f.GetHello(); h != nil {
				hello = h
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("waiting for hello: %v", err)
	}
	return hello
}

func (ss *testStream) waitFrame(t *testing.T, ctx context.Context, what string, pred func(*agentprotocolv1.WardenFrame) bool) *agentprotocolv1.WardenFrame {
	t.Helper()
	var found *agentprotocolv1.WardenFrame
	err := waitFor(ctx, func() bool {
		for _, f := range ss.snapshot() {
			if pred(f) {
				found = f
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
	return found
}

func waitFor(ctx context.Context, cond func() bool) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx, cancel
}

type handshake struct {
	server *testServer
	stream *testStream
	warden *fakewarden.Warden
}

// startWarden starts a warden against a fresh server, waits for its Hello,
// and answers it when ack is true.
func startWarden(t *testing.T, ack bool) *handshake {
	t.Helper()
	ctx, _ := testContext(t)
	s := newTestServer(t)
	w := fakewarden.New(t, testRunID, s.dial)
	w.Start()
	t.Cleanup(w.Stop)
	ss := s.waitStream(t, ctx, 1)
	ss.waitHello(t, ctx)
	if ack {
		ss.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "sk-test"))
	}
	return &handshake{server: s, stream: ss, warden: w}
}

func helloAckFrame(kind agentprotocolv1.LLMCredentialKind, value string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: testRunID,
		Body: &agentprotocolv1.ControlFrame_HelloAck{
			HelloAck: &agentprotocolv1.HelloAck{
				LlmCredential: &agentprotocolv1.LLMCredential{Kind: kind, Value: value},
			},
		},
	}
}

func deliverFrame(deliveryID string, seqs ...int64) *agentprotocolv1.ControlFrame {
	batch := make([]*agentprotocolv1.InboxMessage, len(seqs))
	for i, seq := range seqs {
		batch[i] = &agentprotocolv1.InboxMessage{
			InboxSeq: seq,
			Content:  &agentprotocolv1.InboxMessage_Text{Text: fmt.Sprintf("message %d", seq)},
		}
	}
	return &agentprotocolv1.ControlFrame{
		RunId: testRunID,
		Body: &agentprotocolv1.ControlFrame_Deliver{
			Deliver: &agentprotocolv1.Deliver{
				DeliveryId: deliveryID,
				Batch:      batch,
				ThroughSeq: seqs[len(seqs)-1],
			},
		},
	}
}

func shutdownFrame() *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: testRunID,
		Body:  &agentprotocolv1.ControlFrame_Shutdown{Shutdown: &agentprotocolv1.Shutdown{Reason: "ended"}},
	}
}

func kinds(frames []*agentprotocolv1.WardenFrame) []string {
	var out []string
	for _, f := range frames {
		switch {
		case f.GetHello() != nil:
			out = append(out, "hello")
		case f.GetAccepted() != nil:
			out = append(out, "accepted")
		case f.GetEvent() != nil && f.GetEvent().GetTurnStarted() != nil:
			out = append(out, "turn_started")
		case f.GetEvent() != nil && f.GetEvent().GetTurnEnded() != nil:
			out = append(out, "turn_ended")
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

func countKind(frames []*agentprotocolv1.WardenFrame, kind string) int {
	n := 0
	for _, k := range kinds(frames) {
		if k == kind {
			n++
		}
	}
	return n
}

func TestHandshakeAndShutdown(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	s := newTestServer(t)
	w := fakewarden.New(t, testRunID, s.dial)
	w.Start()
	t.Cleanup(w.Stop)

	ss := s.waitStream(t, ctx, 1)
	hello := ss.waitHello(t, ctx)
	if hello.GetAgentState().GetPhase() != agentprotocolv1.Phase_PHASE_STARTING {
		t.Errorf("initial phase = %v, want starting", hello.GetAgentState().GetPhase())
	}
	if hello.GetLastAcceptedSeq() != 0 {
		t.Errorf("initial last_accepted_seq = %d, want 0", hello.GetLastAcceptedSeq())
	}
	if f := ss.snapshot()[0]; f.GetRunId() != testRunID {
		t.Errorf("hello run_id = %q, want %q", f.GetRunId(), testRunID)
	}

	ss.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_OAUTH_TOKEN, "token-1"))
	if err := w.WaitFor(ctx, func() bool { return w.ReceivedHelloAck().Received }); err != nil {
		t.Fatalf("waiting for hello ack: %v", err)
	}
	got := w.ReceivedHelloAck()
	if got.Kind != agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_OAUTH_TOKEN || got.Value != "token-1" {
		t.Errorf("hello ack = %+v, want oauth token token-1", got)
	}

	ss.send(shutdownFrame())
	if err := w.WaitFor(ctx, w.ShutdownReceived); err != nil {
		t.Fatalf("waiting for shutdown: %v", err)
	}
	time.Sleep(4 * reconnectSettle)
	if got := w.Connects(); got != 1 {
		t.Errorf("connects after shutdown = %d, want 1", got)
	}
}

// reconnectSettle is longer than the fake's maximum reconnect backoff, so a
// short wait after a terminal event is enough to see that no reconnect
// happened.
const reconnectSettle = 60 * time.Millisecond

func TestDedupe(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	h := startWarden(t, true)

	h.stream.send(deliverFrame("d1", 1, 2))
	h.stream.waitFrame(t, ctx, "first accepted", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetAccepted() != nil
	})
	if err := h.warden.WaitFor(ctx, func() bool { return len(h.warden.Written()) == 2 }); err != nil {
		t.Fatalf("waiting for writes: %v", err)
	}
	if got := h.warden.Written(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("written = %v, want [1 2]", got)
	}
	_ = h.stream.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetEvent().GetTurnEnded() != nil
	})

	want := []string{"hello", "accepted", "turn_started", "turn_ended"}
	if got := kinds(h.stream.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}

	// The same batch again: written once, acknowledged again, no second turn.
	h.stream.send(deliverFrame("d1", 1, 2))
	if err := h.warden.WaitFor(ctx, func() bool {
		return countKind(h.stream.snapshot(), "accepted") == 2
	}); err != nil {
		t.Fatalf("waiting for second accepted: %v", err)
	}
	if got := h.warden.Written(); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("written after duplicate = %v, want [1 2]", got)
	}
	if got := len(h.warden.Turns()); got != 1 {
		t.Errorf("turns after duplicate = %d, want 1", got)
	}

	// A batch entirely below last_accepted_seq is acknowledged without a turn.
	h.stream.send(deliverFrame("d2", 1, 2))
	if err := h.warden.WaitFor(ctx, func() bool {
		return countKind(h.stream.snapshot(), "accepted") == 3
	}); err != nil {
		t.Fatalf("waiting for third accepted: %v", err)
	}
	if got := len(h.warden.Turns()); got != 1 {
		t.Errorf("turns after stale batch = %d, want 1", got)
	}
	if got := kinds(h.stream.snapshot()); !slices.Equal(got, []string{"hello", "accepted", "turn_started", "turn_ended", "accepted", "accepted"}) {
		t.Errorf("frames = %v", got)
	}
}

func TestTurns(t *testing.T) {
	t.Parallel()

	t.Run("manual end and deliver during a turn", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		s := newTestServer(t)
		w := fakewarden.New(t, testRunID, s.dial)
		w.SetAutoEndTurns(false)
		w.Start()
		t.Cleanup(w.Stop)
		ss := s.waitStream(t, ctx, 1)
		ss.waitHello(t, ctx)
		ss.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))

		ss.send(deliverFrame("d1", 1))
		ss.waitFrame(t, ctx, "turn started", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetEvent().GetTurnStarted() != nil
		})
		ss.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetAccepted() != nil
		})

		// A delivery during the turn is written and acknowledged but starts no
		// new turn.
		ss.send(deliverFrame("d2", 2))
		if err := w.WaitFor(ctx, func() bool {
			return countKind(ss.snapshot(), "accepted") == 2
		}); err != nil {
			t.Fatalf("waiting for second accepted: %v", err)
		}
		if got := countKind(ss.snapshot(), "turn_started"); got != 1 {
			t.Errorf("turn_started frames = %d, want 1", got)
		}
		if got := w.Written(); !slices.Equal(got, []int64{1, 2}) {
			t.Errorf("written = %v, want [1 2]", got)
		}
		if got := len(w.Turns()); got != 1 {
			t.Fatalf("turns = %d, want 1", got)
		}

		w.EndTurn("done", true)
		ss.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			ended := f.GetEvent().GetTurnEnded()
			return ended != nil && ended.GetOutput() == "done" && ended.GetIsError()
		})
		turns := w.Turns()
		if !turns[0].Ended || turns[0].Output != "done" || !turns[0].IsError || turns[0].DeliveryID != "d1" {
			t.Errorf("turn = %+v, want ended d1 with output done", turns[0])
		}

		// The next delivery starts a fresh turn.
		ss.send(deliverFrame("d3", 3))
		if err := w.WaitFor(ctx, func() bool { return len(w.Turns()) == 2 }); err != nil {
			t.Fatalf("waiting for third turn: %v", err)
		}
		if got := w.Turns()[1].DeliveryID; got != "d3" {
			t.Errorf("second turn delivery = %q, want d3", got)
		}
		if err := w.WaitFor(ctx, func() bool {
			return countKind(ss.snapshot(), "turn_started") == 2
		}); err != nil {
			t.Fatalf("waiting for second turn_started: %v", err)
		}
	})

	t.Run("auto end", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.stream.send(deliverFrame("d1", 1))
		h.stream.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetEvent().GetTurnEnded() != nil
		})
		want := []string{"hello", "accepted", "turn_started", "turn_ended"}
		if got := kinds(h.stream.snapshot()); !slices.Equal(got, want) {
			t.Errorf("frames = %v, want %v", got, want)
		}
		turns := h.warden.Turns()
		if len(turns) != 1 || !turns[0].Ended || turns[0].AgentExited {
			t.Errorf("turns = %+v, want one ended turn", turns)
		}
	})
}

func TestDropNext(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.warden.DropNext(fakewarden.DropAccepted)
		h.stream.send(deliverFrame("d1", 1))
		h.stream.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetEvent().GetTurnEnded() != nil
		})
		if got := countKind(h.stream.snapshot(), "accepted"); got != 0 {
			t.Errorf("accepted frames = %d, want 0", got)
		}
		if got := h.warden.Written(); !slices.Equal(got, []int64{1}) {
			t.Errorf("written = %v, want [1]", got)
		}

		h.warden.Disconnect()
		ss2 := h.server.waitStream(t, ctx, 2)
		hello := ss2.waitHello(t, ctx)
		if got := hello.GetLastAcceptedSeq(); got != 1 {
			t.Errorf("hello last_accepted_seq = %d, want 1", got)
		}
	})

	t.Run("deliver", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.warden.DropNext(fakewarden.DropDeliver)
		h.stream.send(deliverFrame("d1", 1))
		time.Sleep(4 * reconnectSettle)
		if got := h.warden.Written(); len(got) != 0 {
			t.Errorf("written = %v, want none", got)
		}
		if got := countKind(h.stream.snapshot(), "accepted"); got != 0 {
			t.Errorf("accepted frames = %d, want 0", got)
		}

		// The next delivery is processed normally.
		h.stream.send(deliverFrame("d1", 1))
		h.stream.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetAccepted() != nil
		})
		if got := h.warden.Written(); !slices.Equal(got, []int64{1}) {
			t.Errorf("written = %v, want [1]", got)
		}
	})

	t.Run("turn ended", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.warden.DropNext(fakewarden.DropTurnEnded)
		h.stream.send(deliverFrame("d1", 1))
		h.stream.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetAccepted() != nil
		})
		if got := countKind(h.stream.snapshot(), "turn_ended"); got != 0 {
			t.Errorf("turn_ended frames = %d, want 0", got)
		}
		turns := h.warden.Turns()
		if len(turns) != 1 || !turns[0].Ended {
			t.Fatalf("turns = %+v, want one ended turn", turns)
		}

		// The state still reached the boundary and last_turn is reported by the
		// next Hello.
		h.warden.Disconnect()
		ss2 := h.server.waitStream(t, ctx, 2)
		hello := ss2.waitHello(t, ctx)
		if got := hello.GetAgentState().GetPhase(); got != agentprotocolv1.Phase_PHASE_BOUNDARY {
			t.Errorf("hello phase = %v, want boundary", got)
		}
		if last := hello.GetLastTurn(); last == nil || last.GetTurnId() != turns[0].TurnID {
			t.Errorf("hello last_turn = %+v, want turn %q", last, turns[0].TurnID)
		}
	})
}

func TestDisconnect(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	h := startWarden(t, true)

	h.stream.send(deliverFrame("d1", 1, 2))
	h.stream.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetEvent().GetTurnEnded() != nil
	})
	if got := h.warden.Connects(); got != 1 {
		t.Fatalf("connects = %d, want 1", got)
	}

	h.warden.Disconnect()
	ss2 := h.server.waitStream(t, ctx, 2)
	hello := ss2.waitHello(t, ctx)
	if got := hello.GetLastAcceptedSeq(); got != 2 {
		t.Errorf("hello last_accepted_seq = %d, want 2", got)
	}
	if got := hello.GetAgentState().GetPhase(); got != agentprotocolv1.Phase_PHASE_BOUNDARY {
		t.Errorf("hello phase = %v, want boundary", got)
	}
	if err := waitFor(ctx, func() bool { return h.warden.Connects() == 2 }); err != nil {
		t.Fatalf("waiting for reconnect: %v", err)
	}

	// The first stream was closed by the warden.
	if err := waitFor(ctx, func() bool {
		select {
		case <-h.stream.ctx.Done():
			return true
		default:
			return false
		}
	}); err != nil {
		t.Errorf("old stream still open: %v", err)
	}
}

func TestFreezeThaw(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	s := newTestServer(t)
	w := fakewarden.New(t, testRunID, s.dial)
	w.SetAutoEndTurns(false)
	w.Start()
	t.Cleanup(w.Stop)

	old := s.waitStream(t, ctx, 1)
	old.waitHello(t, ctx)
	old.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))

	old.send(deliverFrame("d1", 1))
	old.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetAccepted() != nil
	})

	w.Freeze()
	old.send(deliverFrame("d2", 2))
	time.Sleep(4 * reconnectSettle)
	if got := w.Written(); !slices.Equal(got, []int64{1}) {
		t.Errorf("written while frozen = %v, want [1]", got)
	}
	if got := countKind(old.snapshot(), "accepted"); got != 1 {
		t.Errorf("accepted frames while frozen = %d, want 1", got)
	}

	w.Thaw()
	next := s.waitStream(t, ctx, 2)
	hello := next.waitHello(t, ctx)
	next.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))
	if got := hello.GetLastAcceptedSeq(); got != 1 {
		t.Errorf("thaw hello last_accepted_seq = %d, want 1", got)
	}
	if got := hello.GetAgentState().GetPhase(); got != agentprotocolv1.Phase_PHASE_IN_TURN {
		t.Errorf("thaw hello phase = %v, want in_turn", got)
	}
	if got := hello.GetAgentState().GetTurnId(); got != w.Turns()[0].TurnID {
		t.Errorf("thaw hello turn_id = %q, want %q", got, w.Turns()[0].TurnID)
	}

	// The control plane has not replaced the old stream yet, so it is still
	// open.
	select {
	case <-old.ctx.Done():
		t.Fatal("old stream closed before the control plane replaced it")
	default:
	}

	// The new stream processes deliveries again.
	next.send(deliverFrame("d2", 2))
	next.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetAccepted() != nil
	})
	if got := w.Written(); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("written after thaw = %v, want [1 2]", got)
	}
	w.EndTurn("ok", false)
	next.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
		return f.GetEvent().GetTurnEnded() != nil
	})

	// Replacing the old stream must not trigger another connection.
	old.closeWith(codes.Aborted)
	if err := waitFor(ctx, func() bool {
		select {
		case <-old.ctx.Done():
			return true
		default:
			return false
		}
	}); err != nil {
		t.Fatalf("old stream did not close: %v", err)
	}
	time.Sleep(4 * reconnectSettle)
	if got := w.Connects(); got != 2 {
		t.Errorf("connects after old stream closed = %d, want 2", got)
	}
	if got := w.LastCloseCode(); got != codes.OK {
		t.Errorf("last close code = %v, want OK (replaced streams must not update it)", got)
	}
}

func TestThawWithoutConnecting(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	h := startWarden(t, true)

	h.warden.Freeze()
	h.warden.SetThawConnects(false)
	h.warden.Thaw()

	if err := waitFor(ctx, func() bool {
		select {
		case <-h.stream.ctx.Done():
			return true
		default:
			return false
		}
	}); err != nil {
		t.Fatalf("old stream was not discarded: %v", err)
	}
	time.Sleep(4 * reconnectSettle)
	if got := h.warden.Connects(); got != 1 {
		t.Errorf("connects = %d, want 1", got)
	}
	if got := h.server.streamCount(); got != 1 {
		t.Errorf("server streams = %d, want 1", got)
	}
}

func TestReconnectAndStop(t *testing.T) {
	t.Parallel()

	t.Run("unavailable reconnects", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.stream.closeWith(codes.Unavailable)
		if err := waitFor(ctx, func() bool { return h.warden.Connects() == 2 }); err != nil {
			t.Fatalf("waiting for reconnect: %v", err)
		}
		ss2 := h.server.waitStream(t, ctx, 2)
		ss2.waitHello(t, ctx)
	})

	t.Run("normal close reconnects", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.stream.closeWith(codes.OK)
		if err := waitFor(ctx, func() bool { return h.warden.Connects() == 2 }); err != nil {
			t.Fatalf("waiting for reconnect: %v", err)
		}
	})

	t.Run("failed precondition stops", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.stream.closeWith(codes.FailedPrecondition)
		if err := h.warden.WaitFor(ctx, func() bool { return h.warden.LastCloseCode() == codes.FailedPrecondition }); err != nil {
			t.Fatalf("waiting for close code: %v", err)
		}
		time.Sleep(4 * reconnectSettle)
		if got := h.warden.Connects(); got != 1 {
			t.Errorf("connects = %d, want 1", got)
		}
	})

	t.Run("shutdown stops", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)
		h.stream.send(shutdownFrame())
		if err := h.warden.WaitFor(ctx, h.warden.ShutdownReceived); err != nil {
			t.Fatalf("waiting for shutdown: %v", err)
		}
		time.Sleep(4 * reconnectSettle)
		if got := h.warden.Connects(); got != 1 {
			t.Errorf("connects = %d, want 1", got)
		}
	})
}

func TestSetDial(t *testing.T) {
	t.Parallel()
	ctx, _ := testContext(t)
	first := newTestServer(t)
	w := fakewarden.New(t, testRunID, first.dial)
	w.Start()
	t.Cleanup(w.Stop)

	ss1 := first.waitStream(t, ctx, 1)
	ss1.waitHello(t, ctx)
	ss1.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))
	if err := waitFor(ctx, func() bool { return w.Connects() == 1 }); err != nil {
		t.Fatalf("waiting for first connect: %v", err)
	}

	second := newTestServer(t)
	w.SetDial(second.dial)
	first.srv.Stop()

	ss2 := second.waitStream(t, ctx, 1)
	ss2.waitHello(t, ctx)
	if f := ss2.snapshot()[0]; f.GetRunId() != testRunID {
		t.Errorf("hello run_id = %q, want %q", f.GetRunId(), testRunID)
	}
	if err := waitFor(ctx, func() bool { return w.Connects() == 2 }); err != nil {
		t.Fatalf("waiting for second connect: %v", err)
	}
}

func TestSpontaneousTurnAndExit(t *testing.T) {
	t.Parallel()

	t.Run("deliver during a spontaneous turn", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)

		h.warden.StartSpontaneousTurn()
		started := h.stream.waitFrame(t, ctx, "spontaneous turn started", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetEvent().GetTurnStarted() != nil
		})
		if got := started.GetEvent().GetTurnStarted().GetDeliveryId(); got != "" {
			t.Errorf("delivery_id = %q, want empty", got)
		}

		h.stream.send(deliverFrame("d1", 1))
		h.stream.waitFrame(t, ctx, "turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetEvent().GetTurnEnded() != nil
		})
		want := []string{"hello", "turn_started", "accepted", "turn_ended"}
		if got := kinds(h.stream.snapshot()); !slices.Equal(got, want) {
			t.Errorf("frames = %v, want %v", got, want)
		}
		turns := h.warden.Turns()
		if len(turns) != 1 || turns[0].DeliveryID != "" || !turns[0].Ended {
			t.Errorf("turns = %+v, want one ended spontaneous turn", turns)
		}
	})

	t.Run("exit at boundary", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		h := startWarden(t, true)

		h.warden.Exit()
		h.stream.waitFrame(t, ctx, "exit turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			ended := f.GetEvent().GetTurnEnded()
			return ended != nil && ended.GetAgentExited()
		})
		want := []string{"hello", "turn_started", "turn_ended"}
		if got := kinds(h.stream.snapshot()); !slices.Equal(got, want) {
			t.Errorf("frames = %v, want %v", got, want)
		}
		turns := h.warden.Turns()
		if len(turns) != 1 || !turns[0].AgentExited {
			t.Errorf("turns = %+v, want one exited turn", turns)
		}

		h.warden.Disconnect()
		ss2 := h.server.waitStream(t, ctx, 2)
		hello := ss2.waitHello(t, ctx)
		if got := hello.GetAgentState().GetPhase(); got != agentprotocolv1.Phase_PHASE_EXITED {
			t.Errorf("hello phase = %v, want exited", got)
		}
	})

	t.Run("exit during a turn", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		s := newTestServer(t)
		w := fakewarden.New(t, testRunID, s.dial)
		w.SetAutoEndTurns(false)
		w.Start()
		t.Cleanup(w.Stop)
		ss := s.waitStream(t, ctx, 1)
		ss.waitHello(t, ctx)
		ss.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))

		ss.send(deliverFrame("d1", 1))
		ss.waitFrame(t, ctx, "accepted", func(f *agentprotocolv1.WardenFrame) bool {
			return f.GetAccepted() != nil
		})
		w.Exit()
		ss.waitFrame(t, ctx, "exit turn ended", func(f *agentprotocolv1.WardenFrame) bool {
			ended := f.GetEvent().GetTurnEnded()
			return ended != nil && ended.GetAgentExited()
		})
		if got := countKind(ss.snapshot(), "turn_started"); got != 1 {
			t.Errorf("turn_started frames = %d, want 1 (no extra turn at exit)", got)
		}
		turns := w.Turns()
		if len(turns) != 1 || !turns[0].AgentExited || turns[0].DeliveryID != "d1" {
			t.Errorf("turns = %+v, want the delivered turn exited", turns)
		}
	})
}

// errTB records Errorf calls. It embeds a nil testing.TB so the unexported
// method set is satisfied; only the methods below may be called.
type errTB struct {
	testing.TB
	mu   sync.Mutex
	msgs []string
}

func (e *errTB) Errorf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msgs = append(e.msgs, fmt.Sprintf(format, args...))
}

func (e *errTB) Helper()             {}
func (e *errTB) Cleanup(func())      {}
func (e *errTB) Name() string        { return "errTB" }
func (e *errTB) Logf(string, ...any) {}
func (e *errTB) Fatalf(format string, args ...any) {
	e.Errorf(format, args...)
}

func (e *errTB) errors() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.msgs...)
}

func TestViolations(t *testing.T) {
	t.Parallel()

	t.Run("deliver before hello ack", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		rec := &errTB{}
		s := newTestServer(t)
		w := fakewarden.New(rec, testRunID, s.dial)
		w.Start()
		t.Cleanup(w.Stop)
		ss := s.waitStream(t, ctx, 1)
		ss.waitHello(t, ctx)

		ss.send(deliverFrame("d1", 1))
		if err := waitFor(ctx, func() bool {
			for _, m := range rec.errors() {
				if strings.Contains(m, "HelloAck") {
					return true
				}
			}
			return false
		}); err != nil {
			t.Fatalf("no violation reported: %v", err)
		}
	})

	t.Run("invalid frame", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testContext(t)
		rec := &errTB{}
		s := newTestServer(t)
		w := fakewarden.New(rec, testRunID, s.dial)
		w.Start()
		t.Cleanup(w.Stop)
		ss := s.waitStream(t, ctx, 1)
		ss.waitHello(t, ctx)
		ss.send(helloAckFrame(agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY, "k"))

		ss.send(&agentprotocolv1.ControlFrame{})
		if err := waitFor(ctx, func() bool {
			for _, m := range rec.errors() {
				if strings.Contains(m, "invalid control frame") {
					return true
				}
			}
			return false
		}); err != nil {
			t.Fatalf("no violation reported: %v", err)
		}
	})
}
