package runcontroller

import (
	"context"
	"errors"
	"io"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// agentEvents 是 AgentStream 服务端向控制器投递的事件入口。它的实现由
// “控制器与 RunService”提供；本文件只管收发、逐帧校验与错误到状态码的映射。
type agentEvents interface {
	// onHello 处理一条新流的首帧。成功时它已登记 c，并把 HelloAck 及随后的
	// Deliver 放入 c 的发送队列。
	onHello(ctx context.Context, c *conn, h *agentprotocolv1.Hello) error
	// onWardenFrame 处理 event 与 accepted。c 已不是当前登记的流时不改任何
	// 状态并返回 nil。
	onWardenFrame(ctx context.Context, c *conn, f *agentprotocolv1.WardenFrame) error
	// onStreamClosed 在流的处理函数返回前调用一次。
	onStreamClosed(ctx context.Context, c *conn)
}

var _ agentprotocolv1.AgentStreamServer = (*agentStreamServer)(nil)

// agentStreamServer 实现 agentprotocolv1.AgentStreamServer。流的建立与替换
// 照 docs/milestone/v0.1.0/agentprotocol.md“建立与替换”一节。
type agentStreamServer struct {
	agentprotocolv1.UnimplementedAgentStreamServer

	ev  agentEvents
	reg *streamRegistry
}

// newAgentStreamServer 构造 AgentStream 服务端。ev 处理事件，reg 是控制器与
// 服务端共用的流登记表；登记与移除由 ev 的回调完成，服务端只持有它。
func newAgentStreamServer(ev agentEvents, reg *streamRegistry) *agentStreamServer {
	return &agentStreamServer{ev: ev, reg: reg}
}

// Connect 处理一条看守进程发起的双向流。首帧必须是校验通过的 Hello，之后
// 每帧先校验、再比对 run_id，然后交给事件入口。接收与发送同时进行，任一
// 结束或 conn 被关闭即结束处理函数；返回前对创建过的 conn 调用一次
// onStreamClosed。
func (s *agentStreamServer) Connect(stream agentprotocolv1.AgentStream_ConnectServer) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return streamEndStatus(ctx, err).Err()
	}
	if err := agentprotocol.ValidateWardenFrame(first); err != nil {
		return status.Errorf(codes.InvalidArgument, "agentstream: invalid first frame: %v", err)
	}
	hello := first.GetHello()
	if hello == nil {
		return status.New(codes.InvalidArgument, "agentstream: first frame is not Hello").Err()
	}

	runID := first.GetRunId()
	c := newConn(runID)
	defer s.ev.onStreamClosed(ctx, c)

	if err := s.ev.onHello(ctx, c, hello); err != nil {
		st := eventErrorStatus(err)
		c.closeWith(st)
		return st.Err()
	}

	pumpErr := make(chan error, 1)
	go func() { pumpErr <- c.pump(ctx, stream.Send) }()

	recvErr := make(chan error, 1)
	go func() { recvErr <- s.receive(ctx, stream, c, runID) }()

	select {
	case err := <-recvErr:
		// 接收先结束：关闭 conn 让 pump 发完已入队的帧后返回。
		st := streamEndStatus(ctx, err)
		c.closeWith(st)
		// 等 pump 发完已入队的帧。接收侧正常结束而发送侧带非 OK 状态时，
		// 以发送侧的状态为准。
		if perr := <-pumpErr; perr != nil && st.Code() == codes.OK {
			st = streamEndStatus(ctx, perr)
		}
		return st.Err()
	case err := <-pumpErr:
		// conn 被关闭（取代、Shutdown、出错）或 ctx 结束。
		return err
	case <-c.done():
		// conn 已关闭：等 pump 把已入队的帧发完，再返回它的关闭状态。
		return <-pumpErr
	}
}

// receive 按顺序读取后续帧。校验与 run_id 比对在交给事件入口之前完成。
func (s *agentStreamServer) receive(
	ctx context.Context,
	stream agentprotocolv1.AgentStream_ConnectServer,
	c *conn,
	runID string,
) error {
	for {
		f, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := agentprotocol.ValidateWardenFrame(f); err != nil {
			return status.Errorf(codes.InvalidArgument, "agentstream: invalid warden frame: %v", err)
		}
		if got := f.GetRunId(); got != runID {
			return status.Errorf(codes.InvalidArgument,
				"agentstream: frame run_id %q does not match stream run_id %q", got, runID)
		}
		if err := s.ev.onWardenFrame(ctx, c, f); err != nil {
			return err
		}
	}
}

// eventErrorStatus 把事件入口返回的错误映射为关闭状态码：ErrNotFound 为
// NotFound，ErrTerminated 为 FailedPrecondition，带 gRPC 状态的错误原样
// 使用，其余为 Unavailable。
func eventErrorStatus(err error) *status.Status {
	if errors.Is(err, ErrNotFound) {
		return status.New(codes.NotFound, err.Error())
	}
	if errors.Is(err, ErrTerminated) {
		return status.New(codes.FailedPrecondition, err.Error())
	}
	if st, ok := status.FromError(err); ok {
		return st
	}
	return status.New(codes.Unavailable, err.Error())
}

// streamEndStatus 把 Recv 返回的错误映射为关闭状态码。对端正常结束
// （io.EOF）为 OK；ctx 结束为它对应的状态；带 gRPC 状态的错误原样使用；
// 其余传输错误为 Unavailable。
func streamEndStatus(ctx context.Context, err error) *status.Status {
	if err == nil || errors.Is(err, io.EOF) {
		return status.New(codes.OK, "")
	}
	if st, ok := status.FromError(err); ok {
		return st
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err)
	}
	return status.New(codes.Unavailable, err.Error())
}
