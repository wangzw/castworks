package controlstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/warden/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const apiKey = agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY

func TestHello(t *testing.T) {
	lastTurn := &agent.TurnResult{TurnID: "turn-9", Output: "result text", IsError: true}
	cases := []struct {
		name       string
		state      agent.State
		wantPhase  agentprotocolv1.Phase
		wantTurnID string
		wantLast   *agent.TurnResult
	}{
		{
			name:      "starting",
			state:     agent.State{Phase: agent.PhaseStarting},
			wantPhase: agentprotocolv1.Phase_PHASE_STARTING,
		},
		{
			name:       "in_turn",
			state:      agent.State{Phase: agent.PhaseInTurn, TurnID: "turn-1", LastAcceptedSeq: 4},
			wantPhase:  agentprotocolv1.Phase_PHASE_IN_TURN,
			wantTurnID: "turn-1",
		},
		{
			name:      "boundary",
			state:     agent.State{Phase: agent.PhaseBoundary, LastAcceptedSeq: 7, LastTurn: lastTurn},
			wantPhase: agentprotocolv1.Phase_PHASE_BOUNDARY,
			wantLast:  lastTurn,
		},
		{
			// exited 时 Hello 不携带 last_turn，即使状态里还留着上一回合。
			name:      "exited",
			state:     agent.State{Phase: agent.PhaseExited, LastAcceptedSeq: 7, LastTurn: lastTurn},
			wantPhase: agentprotocolv1.Phase_PHASE_EXITED,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
				if attempt != 1 {
					t.Errorf("unexpected reconnect, attempt %d", attempt)
				}
				if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
					return err
				}
				return st.Send(shutdownFrame(testRunID))
			}
			srv := newTestServer(t, testRunID, nil, handler)
			c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Run(ctx, newFakeAgent(tc.state)); err != nil {
				t.Fatalf("Run: %v", err)
			}

			hello := srv.helloAt(0)
			if hello == nil {
				t.Fatal("server did not receive a hello")
			}
			if got := hello.GetAgentState().GetPhase(); got != tc.wantPhase {
				t.Errorf("hello phase = %v, want %v", got, tc.wantPhase)
			}
			if got := hello.GetAgentState().GetTurnId(); got != tc.wantTurnID {
				t.Errorf("hello turn_id = %q, want %q", got, tc.wantTurnID)
			}
			if got := hello.GetLastAcceptedSeq(); got != tc.state.LastAcceptedSeq {
				t.Errorf("hello last_accepted_seq = %d, want %d", got, tc.state.LastAcceptedSeq)
			}
			if tc.wantLast == nil {
				if hello.GetLastTurn() != nil {
					t.Errorf("hello last_turn = %v, want nil", hello.GetLastTurn())
				}
				return
			}
			lt := hello.GetLastTurn()
			if lt == nil {
				t.Fatal("hello has no last_turn")
			}
			if lt.GetTurnId() != tc.wantLast.TurnID || lt.GetOutput() != tc.wantLast.Output || lt.GetIsError() != tc.wantLast.IsError {
				t.Errorf("hello last_turn = %+v, want %+v", lt, tc.wantLast)
			}
		})
	}
}

// runDeliverCase 启动一个只接受一条流的服务端，发送 frames 后等待
// wantAccepted 条 Accepted，再发 Shutdown。
func runDeliverCase(t *testing.T, frames []*agentprotocolv1.ControlFrame, wantAccepted int) (*fakeAgent, []*agentprotocolv1.Accepted) {
	t.Helper()
	accepted := make(chan *agentprotocolv1.Accepted, 8)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if attempt != 1 {
			t.Errorf("unexpected reconnect, attempt %d", attempt)
		}
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		for _, f := range frames {
			if err := st.Send(f); err != nil {
				return err
			}
		}
		for i := 0; i < wantAccepted; i++ {
			f, err := st.Recv()
			if err != nil {
				return err
			}
			if verr := agentprotocol.ValidateWardenFrame(f); verr != nil {
				t.Errorf("invalid warden frame: %v", verr)
				return nil
			}
			a := f.GetAccepted()
			if a == nil {
				t.Errorf("warden frame body = %T, want accepted", f.GetBody())
				return nil
			}
			accepted <- a
		}
		return st.Send(shutdownFrame(testRunID))
	}
	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseBoundary})
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ag.sink = c.Sink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, ag); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(accepted)
	out := make([]*agentprotocolv1.Accepted, 0, wantAccepted)
	for a := range accepted {
		out = append(out, a)
	}
	return ag, out
}

func TestDeliverDedupRepeatedBatch(t *testing.T) {
	ag, accepted := runDeliverCase(t, []*agentprotocolv1.ControlFrame{
		deliverFrame(testRunID, "d1", inbox(1, "a"), inbox(2, "b")),
		deliverFrame(testRunID, "d2", inbox(1, "a"), inbox(2, "b")),
	}, 2)

	batches := ag.recordedBatches()
	if len(batches) != 1 {
		t.Fatalf("Deliver called %d times, want 1: %+v", len(batches), batches)
	}
	if batches[0].DeliveryID != "d1" || batches[0].ThroughSeq != 2 {
		t.Errorf("batch = %+v, want delivery d1 through 2", batches[0])
	}
	if got := strings.Join(batches[0].Texts, ","); got != "a,b" {
		t.Errorf("batch texts = %q, want %q", got, "a,b")
	}

	if len(accepted) != 2 {
		t.Fatalf("received %d accepted frames, want 2", len(accepted))
	}
	if got := accepted[0]; got.GetDeliveryId() != "d1" || got.GetThroughSeq() != 2 {
		t.Errorf("first accepted = %+v, want d1 through 2", got)
	}
	if got := accepted[1]; got.GetDeliveryId() != "d2" || got.GetThroughSeq() != 2 {
		t.Errorf("second accepted = %+v, want d2 through 2", got)
	}

	creds := ag.recordedCreds()
	if len(creds) != 1 || creds[0].Kind != agent.CredentialAPIKey || creds[0].Value != "credential-value" {
		t.Errorf("recorded credentials = %+v, want the api key from hello_ack", creds)
	}
}

func TestDeliverDedupPartialOverlap(t *testing.T) {
	ag, accepted := runDeliverCase(t, []*agentprotocolv1.ControlFrame{
		deliverFrame(testRunID, "d1", inbox(1, "a"), inbox(2, "b")),
		deliverFrame(testRunID, "d2", inbox(2, "b"), inbox(3, "c")),
	}, 2)

	batches := ag.recordedBatches()
	if len(batches) != 2 {
		t.Fatalf("Deliver called %d times, want 2: %+v", len(batches), batches)
	}
	if got := strings.Join(batches[0].Texts, ","); got != "a,b" {
		t.Errorf("first batch texts = %q, want %q", got, "a,b")
	}
	if got := strings.Join(batches[1].Texts, ","); got != "c" {
		t.Errorf("second batch texts = %q, want %q", got, "c")
	}
	if batches[1].DeliveryID != "d2" || batches[1].ThroughSeq != 3 {
		t.Errorf("second batch = %+v, want delivery d2 through 3", batches[1])
	}

	if len(accepted) != 2 {
		t.Fatalf("received %d accepted frames, want 2", len(accepted))
	}
	if got := accepted[0]; got.GetDeliveryId() != "d1" || got.GetThroughSeq() != 2 {
		t.Errorf("first accepted = %+v, want d1 through 2", got)
	}
	if got := accepted[1]; got.GetDeliveryId() != "d2" || got.GetThroughSeq() != 3 {
		t.Errorf("second accepted = %+v, want d2 through 3", got)
	}
}

func TestDeliverExitedIsNotAcknowledged(t *testing.T) {
	var (
		gotMu sync.Mutex
		got   []*agentprotocolv1.WardenFrame
	)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if attempt != 1 {
			t.Errorf("unexpected reconnect, attempt %d", attempt)
		}
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		if err := st.Send(deliverFrame(testRunID, "d1", inbox(1, "a"))); err != nil {
			return err
		}
		// 驱动器返回 ErrExited 时客户端不回 Accepted；给足时间看有没有帧上来。
		recv := make(chan *agentprotocolv1.WardenFrame, 1)
		go func() {
			f, err := st.Recv()
			if err == nil {
				recv <- f
			}
		}()
		select {
		case f := <-recv:
			gotMu.Lock()
			got = append(got, f)
			gotMu.Unlock()
		case <-time.After(300 * time.Millisecond):
		}
		return st.Send(shutdownFrame(testRunID))
	}
	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseExited})
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, ag); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(ag.recordedBatches()); got != 1 {
		t.Fatalf("Deliver called %d times, want 1", got)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	for _, f := range got {
		t.Errorf("unexpected frame sent after ErrExited: %T", f.GetBody())
	}
}

// TestDeliverFailureIsNotAcknowledged 守住“写入失败不回调 Accepted”：驱动器
// 返回错误时不发 Accepted、last_accepted_seq 不前进，下一次 Hello 仍带旧值。
func TestDeliverFailureIsNotAcknowledged(t *testing.T) {
	var (
		gotMu sync.Mutex
		got   []*agentprotocolv1.WardenFrame
	)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		switch attempt {
		case 1:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			// 这两个 inbox_seq 都大于旧的 last_accepted_seq=4。
			if err := st.Send(deliverFrame(testRunID, "d1", inbox(5, "a"), inbox(6, "b"))); err != nil {
				return err
			}
			// 写入失败时客户端不回 Accepted；给足时间看有没有帧上来。
			recv := make(chan *agentprotocolv1.WardenFrame, 4)
			go func() {
				for {
					f, err := st.Recv()
					if err != nil {
						return
					}
					recv <- f
				}
			}()
			select {
			case f := <-recv:
				gotMu.Lock()
				got = append(got, f)
				gotMu.Unlock()
			case <-time.After(300 * time.Millisecond):
			}
			return status.Error(codes.Unavailable, "reconnect")
		case 2:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return st.Send(shutdownFrame(testRunID))
		default:
			t.Errorf("unexpected connection attempt %d", attempt)
			return nil
		}
	}
	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseBoundary, LastAcceptedSeq: 4})
	ag.deliverErr = errors.New("write to stdin failed")
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ag.sink = c.Sink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, ag); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := len(ag.recordedBatches()); n != 1 {
		t.Fatalf("Deliver called %d times, want 1", n)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	for _, f := range got {
		t.Errorf("warden frame sent after a failed delivery: %T", f.GetBody())
	}
	ag.mu.Lock()
	lastSeq := ag.state.LastAcceptedSeq
	ag.mu.Unlock()
	if lastSeq != 4 {
		t.Errorf("state last_accepted_seq = %d, want 4 (unchanged)", lastSeq)
	}
	hello := srv.helloAt(1)
	if hello == nil {
		t.Fatal("server did not receive the hello after reconnecting")
	}
	if got := hello.GetLastAcceptedSeq(); got != 4 {
		t.Errorf("hello after failed delivery has last_accepted_seq = %d, want 4", got)
	}
}

func TestStreamCloseCodes(t *testing.T) {
	cases := []struct {
		name      string
		code      codes.Code
		wantTries int
		wantLog   bool
	}{
		{name: "not_found", code: codes.NotFound, wantTries: 2},
		{name: "failed_precondition", code: codes.FailedPrecondition, wantTries: 1},
		{name: "aborted", code: codes.Aborted, wantTries: 2},
		{name: "unavailable", code: codes.Unavailable, wantTries: 2},
		{name: "invalid_argument", code: codes.InvalidArgument, wantTries: 2, wantLog: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs *logCapture
			if tc.wantLog {
				logs = captureLogs(t)
			}
			handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
				if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
					return err
				}
				if attempt == 1 {
					return status.Error(tc.code, "test close")
				}
				return st.Send(shutdownFrame(testRunID))
			}
			srv := newTestServer(t, testRunID, nil, handler)
			c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Run(ctx, newFakeAgent(agent.State{Phase: agent.PhaseBoundary})); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := srv.attemptCount(); got != tc.wantTries {
				t.Errorf("connection attempts = %d, want %d", got, tc.wantTries)
			}
			if tc.wantLog {
				line := ""
				for _, l := range strings.Split(logs.String(), "\n") {
					if strings.Contains(l, "InvalidArgument") {
						line = l
						break
					}
				}
				switch {
				case line == "":
					t.Errorf("logs do not record the InvalidArgument close:\n%s", logs.String())
				case !strings.Contains(line, "level=ERROR"):
					t.Errorf("InvalidArgument close is not logged at ERROR: %s", line)
				}
			}
		})
	}
}

func TestShutdownEndsRun(t *testing.T) {
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if attempt != 1 {
			t.Errorf("unexpected reconnect, attempt %d", attempt)
		}
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		return st.Send(shutdownFrame(testRunID))
	}
	srv := newTestServer(t, testRunID, nil, handler)
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, newFakeAgent(agent.State{Phase: agent.PhaseBoundary})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := srv.attemptCount(); got != 1 {
		t.Errorf("connection attempts = %d, want 1", got)
	}
}

// currentStream 返回当前流，没有时返回 nil。
func currentStream(c *Client) *stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// TestSendEOFDoesNotOverrideCloseStatus 守住“发送侧遇到 io.EOF 不记错误、不
// 取消流，关闭状态码以接收侧为准”。服务端收到 Hello 后立即以
// FailedPrecondition 关流，客户端此时停在 Deliver 里，发送协程先看到
// io.EOF；若它记下 io.EOF 并取消流，接收侧会拿到 Canceled，Run 会误判为
// 需要重连。
func TestSendEOFDoesNotOverrideCloseStatus(t *testing.T) {
	deliverEntered := make(chan struct{})
	releaseDeliver := make(chan struct{})

	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if attempt != 1 {
			t.Errorf("client reconnected after FailedPrecondition (attempt %d); the send-side io.EOF overrode the close status", attempt)
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return st.Send(shutdownFrame(testRunID))
		}
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		if err := st.Send(deliverFrame(testRunID, "d1", inbox(1, "a"))); err != nil {
			return err
		}
		// 立即以 FailedPrecondition 关流。客户端收到 Deliver 后停在
		// Deliver 里，不会调用 Recv，所以接收侧要等发送协程回来才读到状态码。
		return status.Error(codes.FailedPrecondition, "run ended")
	}
	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseBoundary})
	ag.deliverHook = func() {
		close(deliverEntered)
		<-releaseDeliver
	}
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ag.sink = c.Sink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx, ag) }()

	select {
	case <-deliverEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Deliver was not called")
	}
	s := currentStream(c)
	if s == nil {
		t.Fatal("no current stream while a delivery is in progress")
	}

	// Sink 持续推事件，直到发送协程看到服务端关流后的 io.EOF 并退出。
	deadline := time.After(5 * time.Second)
flood:
	for {
		select {
		case <-s.done:
			break flood
		case <-deadline:
			t.Fatal("send loop did not observe the closed stream")
		default:
			c.Sink().TurnStarted("turn-live", "", "session")
		}
	}
	close(releaseDeliver)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after FailedPrecondition")
	}
	if got := srv.attemptCount(); got != 1 {
		t.Errorf("server saw %d connections, want 1", got)
	}
}

func TestInvalidControlFramesReconnect(t *testing.T) {
	cases := []struct {
		name  string
		first *agentprotocolv1.ControlFrame
	}{
		{
			name:  "frame_before_hello_ack",
			first: deliverFrame(testRunID, "d1", inbox(1, "a")),
		},
		{
			name:  "invalid_hello_ack",
			first: helloAckFrame(testRunID, agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_UNSPECIFIED),
		},
		{
			name:  "run_id_mismatch",
			first: helloAckFrame("another-run", apiKey),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
				if attempt == 1 {
					if err := st.Send(tc.first); err != nil {
						return err
					}
					return nil
				}
				if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
					return err
				}
				return st.Send(shutdownFrame(testRunID))
			}
			srv := newTestServer(t, testRunID, nil, handler)
			c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Run(ctx, newFakeAgent(agent.State{Phase: agent.PhaseBoundary})); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := srv.attemptCount(); got != 2 {
				t.Errorf("connection attempts = %d, want 2", got)
			}
		})
	}
}

func TestEventsDuringDisconnectAreDropped(t *testing.T) {
	var (
		sleepCh      = make(chan time.Duration, 8)
		release      = make(chan struct{})
		helloAcked   = make(chan struct{}, 1)
		frameMu      sync.Mutex
		secondFrames []*agentprotocolv1.WardenFrame
	)

	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		switch attempt {
		case 1:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "gone")
		case 2:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			helloAcked <- struct{}{}
			f, err := st.Recv()
			if err != nil {
				return err
			}
			frameMu.Lock()
			secondFrames = append(secondFrames, f)
			frameMu.Unlock()
			return st.Send(shutdownFrame(testRunID))
		default:
			t.Errorf("unexpected attempt %d", attempt)
			return nil
		}
	}

	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseStarting})
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Hour}})
	c.sleep = func(ctx context.Context, d time.Duration) error {
		sleepCh <- d
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	sink := c.Sink()

	errCh := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { errCh <- c.Run(ctx, ag) }()

	select {
	case <-sleepCh:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not enter backoff after the first stream failed")
	}

	// 断线期间状态发生变化，事件没有当前流可入队，应当被丢弃。
	ag.setState(func(st *agent.State) {
		st.Phase = agent.PhaseBoundary
		st.LastAcceptedSeq = 3
		st.LastTurn = &agent.TurnResult{TurnID: "old-turn", Output: "done", IsError: true}
	})
	sink.TurnStarted("stale-turn", "", "session")
	sink.Accepted("stale-delivery", 3)
	close(release)

	select {
	case <-helloAcked:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reconnect")
	}
	// 重连后的实时事件应当排在 Hello 之后正常发出。
	sink.TurnStarted("live-turn", "", "session")

	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}

	hello := srv.helloAt(1)
	if hello == nil {
		t.Fatal("server did not receive the second hello")
	}
	if got := hello.GetAgentState().GetPhase(); got != agentprotocolv1.Phase_PHASE_BOUNDARY {
		t.Errorf("second hello phase = %v, want boundary", got)
	}
	if got := hello.GetLastAcceptedSeq(); got != 3 {
		t.Errorf("second hello last_accepted_seq = %d, want 3", got)
	}
	if lt := hello.GetLastTurn(); lt == nil || lt.GetTurnId() != "old-turn" {
		t.Errorf("second hello last_turn = %v, want old-turn", lt)
	}

	frameMu.Lock()
	defer frameMu.Unlock()
	if len(secondFrames) != 1 {
		t.Fatalf("server received %d frames after hello, want 1", len(secondFrames))
	}
	ev := secondFrames[0].GetEvent()
	if ev == nil || ev.GetTurnId() != "live-turn" {
		t.Errorf("frame after hello = %+v, want the live turn_started", secondFrames[0])
	}
	if ts := ev.GetTurnStarted(); ts == nil || ts.GetDeliveryId() != "" {
		t.Errorf("turn_started = %+v, want an empty delivery_id", ts)
	}
}

// snapshotAgent 在状态锁内、fn 前后各留一个钩子，模拟驱动器在同一个状态
// 变化里既改状态又发事件。
type snapshotAgent struct {
	*fakeAgent
	before func()
	after  func()
}

func (a *snapshotAgent) Snapshot(fn func(agent.State)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.before != nil {
		a.before()
	}
	fn(a.state)
	if a.after != nil {
		a.after()
	}
}

// TestHelloPrecedesEventsAroundSnapshot 守住 Hello 与切换当前流的原子性：
// 切换之前发出的事件没有当前流可入队，被丢弃；切换之后发出的事件排在
// Hello 之后，绝不可能抢在 Hello 前面。
func TestHelloPrecedesEventsAroundSnapshot(t *testing.T) {
	var (
		gotMu sync.Mutex
		got   []*agentprotocolv1.WardenFrame
	)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if attempt != 1 {
			t.Errorf("unexpected reconnect, attempt %d", attempt)
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return st.Send(shutdownFrame(testRunID))
		}
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		recv := make(chan *agentprotocolv1.WardenFrame, 8)
		go func() {
			for {
				f, err := st.Recv()
				if err != nil {
					return
				}
				recv <- f
			}
		}()
		// 留足时间收全 Hello 之后的所有帧；正确实现只有 after-snapshot 一个。
		timer := time.NewTimer(300 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case f := <-recv:
				gotMu.Lock()
				got = append(got, f)
				gotMu.Unlock()
			case <-timer.C:
				return st.Send(shutdownFrame(testRunID))
			}
		}
	}
	srv := newTestServer(t, testRunID, nil, handler)
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	ag := &snapshotAgent{fakeAgent: newFakeAgent(agent.State{Phase: agent.PhaseBoundary, LastAcceptedSeq: 2})}
	ag.before = func() { c.Sink().TurnStarted("dropped-before", "", "session") }
	ag.after = func() { c.Sink().TurnEnded("after-snapshot", "ok", false, false) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, ag); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if bodies := srv.firstFrameBodies(); len(bodies) != 1 || bodies[0] != "hello" {
		t.Errorf("first frames = %v, want [hello]", bodies)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	if len(got) != 1 {
		t.Fatalf("server received %d frames after hello, want 1: %+v", len(got), got)
	}
	ev := got[0].GetEvent()
	if ev == nil || ev.GetTurnId() != "after-snapshot" {
		t.Errorf("frame after hello = %+v, want the after-snapshot event", got[0])
	}
}

// TestHelloIsFirstFrameUnderConcurrentSink 让 Sink 事件与多次流切换并发，
// 守住每条流的首帧都是 Hello：切换当前流与 Hello 入队必须是一个原子步骤。
func TestHelloIsFirstFrameUnderConcurrentSink(t *testing.T) {
	const attempts = 16
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		if attempt < attempts {
			// 给并发事件一个与切换当前流重叠的机会。
			time.Sleep(2 * time.Millisecond)
			return status.Error(codes.Unavailable, "reconnect")
		}
		return st.Send(shutdownFrame(testRunID))
	}
	srv := newTestServer(t, testRunID, nil, handler)
	ag := newFakeAgent(agent.State{Phase: agent.PhaseBoundary})
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: []time.Duration{time.Millisecond}})
	sink := c.Sink()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sink.TurnStarted("turn-live", "", "session")
					time.Sleep(50 * time.Microsecond)
				}
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Run(ctx, ag)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := srv.attemptCount(); got != attempts {
		t.Errorf("connection attempts = %d, want %d", got, attempts)
	}
	bodies := srv.firstFrameBodies()
	if len(bodies) == 0 {
		t.Fatal("server received no frames")
	}
	for i, body := range bodies {
		if body != "hello" {
			t.Errorf("stream %d first frame = %s, want hello", i, body)
		}
	}
}

func TestBackoffSequence(t *testing.T) {
	sleepCh := make(chan time.Duration, 16)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, attempt int) error {
		switch attempt {
		case 1, 2, 4:
			return status.Error(codes.Unavailable, "down")
		case 3:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "down")
		default:
			if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
				return err
			}
			return st.Send(shutdownFrame(testRunID))
		}
	}
	srv := newTestServer(t, testRunID, nil, handler)
	backoff := []time.Duration{11 * time.Millisecond, 22 * time.Millisecond, 44 * time.Millisecond, 55 * time.Millisecond}
	c := New(Config{Address: srv.addr, RunID: testRunID, Backoff: backoff})
	c.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case sleepCh <- d:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx, newFakeAgent(agent.State{Phase: agent.PhaseBoundary})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(sleepCh)

	// 第 3 次尝试收到 HelloAck，退避复位，所以是 11, 22 再来一遍。
	want := []time.Duration{11 * time.Millisecond, 22 * time.Millisecond, 11 * time.Millisecond, 22 * time.Millisecond}
	var got []time.Duration
	for d := range sleepCh {
		got = append(got, d)
	}
	if len(got) != len(want) {
		t.Fatalf("backoff delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff delays = %v, want %v", got, want)
		}
	}
	if attempts := srv.attemptCount(); attempts != 5 {
		t.Errorf("connection attempts = %d, want 5", attempts)
	}
}

func TestDefaultBackoff(t *testing.T) {
	c := New(Config{Address: "127.0.0.1:1", RunID: testRunID})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if got := c.backoffAt(i); got != w {
			t.Fatalf("backoffAt(%d) = %v, want %v", i, got, w)
		}
	}
}

// TestKeepalive 按真实时间运行约 45 s：流空闲 25 s 不被服务端以
// too_many_pings 断开；冻结代理后 20 s 内靠客户端 keepalive 发现失效并重连。
func TestKeepalive(t *testing.T) {
	helloCh := make(chan int, 16)
	handler := func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], _ *agentprotocolv1.Hello, _ int) error {
		if err := st.Send(helloAckFrame(testRunID, apiKey)); err != nil {
			return err
		}
		select {
		case helloCh <- 1:
		default:
		}
		<-st.Context().Done()
		return st.Context().Err()
	}
	srv := newTestServer(t, testRunID, []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}, handler)
	proxy := newFreezeProxy(t, srv.addr)

	c := New(Config{
		Address: proxy.Addr(),
		RunID:   testRunID,
		Backoff: []time.Duration{100 * time.Millisecond},
	})
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { errCh <- c.Run(ctx, newFakeAgent(agent.State{Phase: agent.PhaseStarting})) }()

	select {
	case <-helloCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no initial stream reached the server")
	}

	time.Sleep(25 * time.Second)
	if got := srv.attemptCount(); got != 1 {
		t.Fatalf("stream re-established during 25s idle (attempts=%d); the server may have sent too_many_pings", got)
	}

	frozenAt := time.Now()
	proxy.Freeze()
	select {
	case <-helloCh:
		if elapsed := time.Since(frozenAt); elapsed > 20*time.Second {
			t.Fatalf("keepalive detected the frozen connection after %v, want at most 20s", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("keepalive did not detect the frozen connection and reconnect within 20s")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
}
