package runcontroller

import (
	"context"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件实现 agentEvents：AgentStream 服务端把一条流的首帧、后续帧与关闭
// 交给这里，规则本身由 applyEvent 在事务内执行。

// onHello 处理一条新流的首帧。它登记 c 并按规则把 HelloAck 与随后的
// Deliver 放入 c 的发送队列。
func (c *Controller) onHello(ctx context.Context, conn *conn, h *agentprotocolv1.Hello) error {
	state := h.GetAgentState()
	if state == nil {
		return status.New(codes.InvalidArgument, "runcontroller: hello has no agent_state").Err()
	}
	_, err := c.applyEvent(ctx, conn.runID(), evHello{
		phase:           state.GetPhase(),
		turnID:          state.GetTurnId(),
		lastAcceptedSeq: h.GetLastAcceptedSeq(),
		lastTurn:        h.GetLastTurn(),
	}, applyOpts{registerConn: conn})
	return err
}

// onWardenFrame 处理 event 与 accepted。c 已不是当前登记的流时，applyEvent
// 在取得行锁后会发现并什么也不改，返回 nil。
func (c *Controller) onWardenFrame(ctx context.Context, conn *conn, f *agentprotocolv1.WardenFrame) error {
	switch body := f.GetBody().(type) {
	case *agentprotocolv1.WardenFrame_Event:
		event := body.Event
		if ts := event.GetTurnStarted(); ts != nil {
			return c.applyFrame(ctx, conn, evTurnStarted{
				turnID:     event.GetTurnId(),
				deliveryID: ts.GetDeliveryId(),
			})
		}
		if te := event.GetTurnEnded(); te != nil {
			return c.applyFrame(ctx, conn, evTurnEnded{
				turnID:      event.GetTurnId(),
				output:      te.GetOutput(),
				isError:     te.GetIsError(),
				agentExited: te.GetAgentExited(),
			})
		}
		return status.New(codes.InvalidArgument, "runcontroller: event has no kind").Err()
	case *agentprotocolv1.WardenFrame_Accepted:
		through := body.Accepted.GetThroughSeq()
		// through_seq must echo the most recent Deliver released on this stream.
		// A stream that has received no Deliver has nothing to accept.
		if last := conn.lastDeliverThrough.Load(); last == 0 || through != last {
			return status.Errorf(codes.InvalidArgument,
				"runcontroller: accepted through_seq %d does not match the last deliver %d", through, last)
		}
		return c.applyFrame(ctx, conn, evAccepted{throughSeq: through})
	case *agentprotocolv1.WardenFrame_Hello:
		// Hello is only valid as the first frame of a stream.
		return status.New(codes.InvalidArgument, "runcontroller: Hello on an established stream").Err()
	default:
		return status.New(codes.InvalidArgument, "runcontroller: warden frame has no body").Err()
	}
}

func (c *Controller) applyFrame(ctx context.Context, conn *conn, ev machineEvent) error {
	_, err := c.applyEvent(ctx, conn.runID(), ev, applyOpts{requireCurrent: conn})
	return err
}

// onStreamClosed 在流的处理函数返回前调用一次。被新流取代的旧流不改变任何
// 状态：applyEvent 在行锁内检查当前登记的流。写库失败时按退避重试；这个
// 写发生在处理函数返回之前，没有可观察的流 ctx，Run 未启动时也没有 Run 的
// ctx，所以重试必须有界，不能无限进行。
func (c *Controller) onStreamClosed(_ context.Context, conn *conn) {
	runID := conn.runID()
	if c.streams.current(runID) != conn {
		return
	}
	// The stream context is already gone; the close must still be persisted.
	c.applyEventRetry(context.Background(), runID, evStreamClosed{},
		applyOpts{requireCurrent: conn}, c.streamCloseLimit())
}
