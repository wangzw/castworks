package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// fakeAgentEvents 是 agentEvents 的替身，记录调用次数，并允许每个方法注入
// 返回值。
type fakeAgentEvents struct {
	onHelloFn        func(context.Context, *conn, *agentprotocolv1.Hello) error
	onWardenFrameFn  func(context.Context, *conn, *agentprotocolv1.WardenFrame) error
	onStreamClosedFn func(context.Context, *conn)

	helloCalls  atomic.Int32
	frameCalls  atomic.Int32
	closedCalls atomic.Int32

	// ackRunID 是默认 onHello 回 HelloAck 时使用的 run_id，由 openWithHello
	// 在开流之前设置。
	ackRunID string

	closedCh   chan struct{}
	closedOnce sync.Once
}

func newFakeAgentEvents() *fakeAgentEvents {
	return &fakeAgentEvents{closedCh: make(chan struct{})}
}

func (f *fakeAgentEvents) onHello(ctx context.Context, c *conn, h *agentprotocolv1.Hello) error {
	f.helloCalls.Add(1)
	if f.onHelloFn != nil {
		return f.onHelloFn(ctx, c, h)
	}
	// 默认回一条 HelloAck，客户端据此确认首帧已被处理。
	return c.enqueue(helloAckFrame(f.ackRunID))
}

func (f *fakeAgentEvents) onWardenFrame(ctx context.Context, c *conn, fr *agentprotocolv1.WardenFrame) error {
	f.frameCalls.Add(1)
	if f.onWardenFrameFn != nil {
		return f.onWardenFrameFn(ctx, c, fr)
	}
	return nil
}

func (f *fakeAgentEvents) onStreamClosed(ctx context.Context, c *conn) {
	f.closedCalls.Add(1)
	if f.onStreamClosedFn != nil {
		f.onStreamClosedFn(ctx, c)
	}
	f.closedOnce.Do(func() { close(f.closedCh) })
}

// waitClosed 等待 onStreamClosed 被调用，并断言它恰好被调用一次。
func (f *fakeAgentEvents) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onStreamClosed 没有被调用")
	}
	if got := f.closedCalls.Load(); got != 1 {
		t.Fatalf("onStreamClosed 被调用了 %d 次，想要恰好 1 次", got)
	}
}

// agentStreamHarness 在 bufconn 上起一个只注册 AgentStream 的 gRPC 服务端。
type agentStreamHarness struct {
	client agentprotocolv1.AgentStreamClient
	ev     *fakeAgentEvents
}

func newAgentStreamHarness(t *testing.T, ev *fakeAgentEvents) *agentStreamHarness {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentprotocolv1.RegisterAgentStreamServer(srv, newAgentStreamServer(ev, newStreamRegistry()))
	go func() { _ = srv.Serve(lis) }()

	cc, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("创建 gRPC 客户端: %v", err)
	}

	t.Cleanup(func() {
		_ = cc.Close()
		srv.Stop()
		_ = lis.Close()
	})

	return &agentStreamHarness{client: agentprotocolv1.NewAgentStreamClient(cc), ev: ev}
}

func (h *agentStreamHarness) open(t *testing.T, ctx context.Context) agentprotocolv1.AgentStream_ConnectClient {
	t.Helper()
	stream, err := h.client.Connect(ctx)
	if err != nil {
		t.Fatalf("打开 AgentStream: %v", err)
	}
	return stream
}

// openWithHello 打开流并完成 Hello 握手，返回时服务端已处理完首帧。
func (h *agentStreamHarness) openWithHello(
	t *testing.T,
	ctx context.Context,
	runID string,
) agentprotocolv1.AgentStream_ConnectClient {
	t.Helper()
	h.ev.ackRunID = runID
	stream := h.open(t, ctx)
	if err := stream.Send(validHelloFrame(runID)); err != nil {
		t.Fatalf("发送 Hello: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("接收 HelloAck: %v", err)
	}
	if ack.GetHelloAck() == nil {
		t.Fatalf("首条控制帧不是 HelloAck: %v", ack)
	}
	return stream
}

func validHelloFrame(runID string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Hello{Hello: &agentprotocolv1.Hello{
			AgentState: &agentprotocolv1.AgentState{Phase: agentprotocolv1.Phase_PHASE_BOUNDARY},
		}},
	}
}

func validEventFrame(runID, turnID string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{
			TurnId: turnID,
			Kind: &agentprotocolv1.AgentEvent_TurnStarted{
				TurnStarted: &agentprotocolv1.TurnStarted{},
			},
		}},
	}
}

func helloAckFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{
			LlmCredential: &agentprotocolv1.LLMCredential{
				Kind:  agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY,
				Value: "test-key",
			},
		}},
	}
}

func deliverFrame(runID, deliveryID string, seq int64, text string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: deliveryID,
			Batch:      []*agentprotocolv1.InboxMessage{{InboxSeq: seq, Text: text}},
			ThroughSeq: seq,
		}},
	}
}

func requireStatus(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	if err == nil {
		t.Fatalf("想得到状态码 %v，实际没有错误", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("错误不是 gRPC 状态: %v", err)
	}
	if st.Code() != want {
		t.Fatalf("状态码是 %v，想要 %v（错误: %v）", st.Code(), want, err)
	}
	return st
}

func TestAgentStreamRejectsBadFirstFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame *agentprotocolv1.WardenFrame
	}{
		{name: "不是 Hello", frame: validEventFrame("run-1", "turn-1")},
		{
			name: "Hello 校验失败",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Hello{Hello: &agentprotocolv1.Hello{
					AgentState: &agentprotocolv1.AgentState{},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newFakeAgentEvents()
			h := newAgentStreamHarness(t, ev)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := h.open(t, ctx)
			_ = stream.Send(tc.frame)

			_, err := stream.Recv()
			requireStatus(t, err, codes.InvalidArgument)
			if got := ev.helloCalls.Load(); got != 0 {
				t.Fatalf("首帧被拒时 onHello 调用了 %d 次，想要 0 次", got)
			}
			if got := ev.closedCalls.Load(); got != 0 {
				t.Fatalf("未建立 conn 时 onStreamClosed 调用了 %d 次，想要 0 次", got)
			}
		})
	}
}

func TestAgentStreamRejectsBadSubsequentFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame *agentprotocolv1.WardenFrame
	}{
		{name: "run_id 不同", frame: validEventFrame("run-2", "turn-1")},
		{
			name: "帧校验失败",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newFakeAgentEvents()
			h := newAgentStreamHarness(t, ev)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := h.openWithHello(t, ctx, "run-1")
			_ = stream.Send(tc.frame)

			_, err := stream.Recv()
			requireStatus(t, err, codes.InvalidArgument)
			ev.waitClosed(t)
			if got := ev.frameCalls.Load(); got != 0 {
				t.Fatalf("后续帧被拒时 onWardenFrame 调用了 %d 次，想要 0 次", got)
			}
		})
	}
}

func TestAgentStreamOnHelloErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "ErrNotFound", err: ErrNotFound, want: codes.NotFound},
		{name: "包装的 ErrTerminated", err: fmt.Errorf("包装: %w", ErrTerminated), want: codes.FailedPrecondition},
		{name: "带状态的错误", err: status.Error(codes.ResourceExhausted, "满了"), want: codes.ResourceExhausted},
		{name: "其他错误", err: errors.New("数据库挂了"), want: codes.Unavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newFakeAgentEvents()
			ev.onHelloFn = func(context.Context, *conn, *agentprotocolv1.Hello) error {
				return tc.err
			}
			h := newAgentStreamHarness(t, ev)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := h.open(t, ctx)
			_ = stream.Send(validHelloFrame("run-1"))

			_, err := stream.Recv()
			requireStatus(t, err, tc.want)
			ev.waitClosed(t)
			if got := ev.helloCalls.Load(); got != 1 {
				t.Fatalf("onHello 调用了 %d 次，想要 1 次", got)
			}
		})
	}
}

func TestAgentStreamConnClosedAborted(t *testing.T) {
	ev := newFakeAgentEvents()
	connCh := make(chan *conn, 1)
	ev.onHelloFn = func(_ context.Context, c *conn, _ *agentprotocolv1.Hello) error {
		if err := c.enqueue(helloAckFrame("run-1")); err != nil {
			return err
		}
		connCh <- c
		return nil
	}

	h := newAgentStreamHarness(t, ev)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := h.openWithHello(t, ctx, "run-1")
	c := <-connCh
	c.closeWith(status.New(codes.Aborted, "被新流取代"))

	_, err := stream.Recv()
	requireStatus(t, err, codes.Aborted)
	ev.waitClosed(t)
}

func TestAgentStreamClientCancelCallsOnStreamClosedOnce(t *testing.T) {
	ev := newFakeAgentEvents()
	h := newAgentStreamHarness(t, ev)

	ctx, cancel := context.WithCancel(context.Background())
	h.openWithHello(t, ctx, "run-1")
	cancel()

	ev.waitClosed(t)
	if got := ev.helloCalls.Load(); got != 1 {
		t.Fatalf("onHello 调用了 %d 次，想要 1 次", got)
	}
}

func TestAgentStreamWardenFrameErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "协议违例", err: status.Error(codes.InvalidArgument, "回合号不连续"), want: codes.InvalidArgument},
		{name: "其他错误", err: errors.New("数据库挂了"), want: codes.Unavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newFakeAgentEvents()
			ev.onWardenFrameFn = func(context.Context, *conn, *agentprotocolv1.WardenFrame) error {
				return tc.err
			}
			h := newAgentStreamHarness(t, ev)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := h.openWithHello(t, ctx, "run-1")
			if err := stream.Send(validEventFrame("run-1", "turn-1")); err != nil {
				t.Fatalf("发送回合事件: %v", err)
			}

			_, err := stream.Recv()
			requireStatus(t, err, tc.want)
			ev.waitClosed(t)
			if got := ev.frameCalls.Load(); got != 1 {
				t.Fatalf("onWardenFrame 调用了 %d 次，想要 1 次", got)
			}
		})
	}
}

func TestAgentStreamSendOrder(t *testing.T) {
	ev := newFakeAgentEvents()
	want := []*agentprotocolv1.ControlFrame{
		helloAckFrame("run-1"),
		deliverFrame("run-1", "delivery-1", 1, "第一条"),
		deliverFrame("run-1", "delivery-2", 2, "第二条"),
	}
	ev.onHelloFn = func(_ context.Context, c *conn, _ *agentprotocolv1.Hello) error {
		for _, f := range want {
			if err := c.enqueue(f); err != nil {
				return err
			}
		}
		return nil
	}

	h := newAgentStreamHarness(t, ev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := h.open(t, ctx)
	if err := stream.Send(validHelloFrame("run-1")); err != nil {
		t.Fatalf("发送 Hello: %v", err)
	}
	for i := range want {
		got, err := stream.Recv()
		if err != nil {
			t.Fatalf("接收第 %d 条控制帧: %v", i, err)
		}
		if !proto.Equal(got, want[i]) {
			t.Fatalf("第 %d 条控制帧是 %v，想要 %v", i, got, want[i])
		}
	}
}
