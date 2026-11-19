package controlstream

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
	"github.com/wangzw/castworks/internal/agentprotocol"
	"github.com/wangzw/castworks/internal/warden/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testRunID = "run-1"

// fakeAgent 是进程内的假 agent.Agent。它的 Deliver 模拟驱动器：记下批次与
// 凭据，更新状态并回调 Sink.Accepted。
type fakeAgent struct {
	mu         sync.Mutex
	state      agent.State
	sink       agent.Sink
	batches    []agent.Batch
	creds      []agent.Credential
	deliverErr error
	// deliverHook 在 Deliver 开始处同步调用，供测试让投递停在半途。
	deliverHook func()
}

func newFakeAgent(st agent.State) *fakeAgent {
	return &fakeAgent{state: st}
}

func (f *fakeAgent) Snapshot(fn func(agent.State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.state)
}

func (f *fakeAgent) Deliver(b agent.Batch, cred agent.Credential) error {
	if f.deliverHook != nil {
		f.deliverHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, b)
	f.creds = append(f.creds, cred)
	if f.deliverErr != nil {
		return f.deliverErr
	}
	if f.state.Phase == agent.PhaseExited {
		return agent.ErrExited
	}
	f.state.LastAcceptedSeq = b.ThroughSeq
	f.state.TurnID = "turn-" + b.DeliveryID
	f.state.Phase = agent.PhaseInTurn
	if f.sink != nil {
		// 与真实驱动器一样，在状态锁内同步回调。
		f.sink.Accepted(b.DeliveryID, b.ThroughSeq)
	}
	return nil
}

func (f *fakeAgent) Shutdown(context.Context) error { return nil }

func (f *fakeAgent) setState(fn func(*agent.State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.state)
}

func (f *fakeAgent) recordedBatches() []agent.Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Batch(nil), f.batches...)
}

func (f *fakeAgent) recordedCreds() []agent.Credential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Credential(nil), f.creds...)
}

// serverHandler 处理一条流。attempt 从 1 开始。
type serverHandler func(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame], hello *agentprotocolv1.Hello, attempt int) error

// testServer 是进程内的假 AgentStream 服务端。它校验客户端首帧是合法的
// Hello，记录每次尝试的 Hello。
type testServer struct {
	agentprotocolv1.UnimplementedAgentStreamServer

	runID   string
	addr    string
	handler serverHandler

	mu          sync.Mutex
	attempts    int
	hellos      []*agentprotocolv1.Hello
	firstFrames []string
	attemptCh   chan int
}

func newTestServer(t *testing.T, runID string, opts []grpc.ServerOption, handler serverHandler) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testServer{runID: runID, handler: handler, attemptCh: make(chan int, 64)}
	gs := grpc.NewServer(opts...)
	agentprotocolv1.RegisterAgentStreamServer(gs, s)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	s.addr = ln.Addr().String()
	return s
}

func (s *testServer) Connect(st grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame]) error {
	frame, err := st.Recv()
	if err != nil {
		return err
	}
	first := frameBodyName(frame)
	s.mu.Lock()
	s.firstFrames = append(s.firstFrames, first)
	s.mu.Unlock()
	if verr := agentprotocol.ValidateWardenFrame(frame); verr != nil {
		return status.Error(codes.InvalidArgument, verr.Error())
	}
	hello := frame.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first frame is not hello")
	}
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	s.hellos = append(s.hellos, hello)
	s.mu.Unlock()
	select {
	case s.attemptCh <- attempt:
	default:
	}
	return s.handler(st, hello, attempt)
}

func (s *testServer) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// firstFrameBodies 返回每条连接收到的首帧 body 类型，按连接先后排列。
func (s *testServer) firstFrameBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.firstFrames...)
}

func frameBodyName(f *agentprotocolv1.WardenFrame) string {
	switch f.GetBody().(type) {
	case *agentprotocolv1.WardenFrame_Hello:
		return "hello"
	case *agentprotocolv1.WardenFrame_Event:
		return "event"
	case *agentprotocolv1.WardenFrame_Accepted:
		return "accepted"
	default:
		return "other"
	}
}

func (s *testServer) helloAt(i int) *agentprotocolv1.Hello {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.hellos) {
		return nil
	}
	return s.hellos[i]
}

func helloAckFrame(runID string, kind agentprotocolv1.LLMCredentialKind) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{
			LlmCredential: &agentprotocolv1.LLMCredential{Kind: kind, Value: "credential-value"},
		}},
	}
}

func shutdownFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body:  &agentprotocolv1.ControlFrame_Shutdown{Shutdown: &agentprotocolv1.Shutdown{Reason: "ended"}},
	}
}

func deliverFrame(runID, deliveryID string, msgs ...*agentprotocolv1.InboxMessage) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: deliveryID,
			Batch:      msgs,
			ThroughSeq: msgs[len(msgs)-1].GetInboxSeq(),
		}},
	}
}

func inbox(seq int64, text string) *agentprotocolv1.InboxMessage {
	return &agentprotocolv1.InboxMessage{InboxSeq: seq, Text: text}
}

// logCapture 收集默认 slog 日志器的输出。
type logCapture struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// freezeProxy 是可控的 TCP 代理。Freeze 之后，冻结前建立的连接只读不转发，
// 连接本身保持打开，因此对端只能靠 keepalive 超时发现失效；冻结之后新建的
// 连接照常转发，重连能够成功。
type freezeProxy struct {
	ln      net.Listener
	backend string

	mu     sync.Mutex
	conns  []*proxyConn
	closed bool
}

type proxyConn struct {
	frozen atomic.Bool
}

func newFreezeProxy(t *testing.T, backend string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &freezeProxy{ln: ln, backend: backend}
	t.Cleanup(p.Close)
	go p.acceptLoop()
	return p
}

func (p *freezeProxy) acceptLoop() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		pc := &proxyConn{}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = client.Close()
			return
		}
		p.conns = append(p.conns, pc)
		p.mu.Unlock()
		go p.relay(client, pc)
	}
}

func (p *freezeProxy) relay(client net.Conn, pc *proxyConn) {
	backend, err := net.Dial("tcp", p.backend)
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		relayDirection(backend, client, pc)
		done <- struct{}{}
	}()
	go func() {
		relayDirection(client, backend, pc)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = backend.Close()
	<-done
}

// relayDirection 在未冻结时转发；冻结后读到的字节直接丢弃。
func relayDirection(dst, src net.Conn, pc *proxyConn) {
	buf := make([]byte, 32<<10)
	for {
		_ = src.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := src.Read(buf)
		if n > 0 && !pc.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

// Freeze 只冻结当前已建立的连接。
func (p *freezeProxy) Freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.frozen.Store(true)
	}
}

func (p *freezeProxy) Addr() string { return p.ln.Addr().String() }

func (p *freezeProxy) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.ln.Close()
}
