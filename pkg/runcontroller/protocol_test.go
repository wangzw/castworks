package runcontroller_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// TestProtocolCloseCodes covers the four status codes agentprotocol.md
// “测试” lists: a first frame that is not Hello, a frame whose run_id differs
// from the stream's, a run_id that does not exist, and a run that has already
// ended or failed.
func TestProtocolCloseCodes(t *testing.T) {
	t.Parallel()

	t.Run("first frame is not hello", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)

		stream := h.openStream()
		send(t, stream, acceptedFrame(runID, 1))
		h.expectCloseCode(stream, codes.InvalidArgument)
	})

	t.Run("run_id differs", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		other := h.createRun("second")
		h.waitCalls(fakeprovider.OpProvision, 2)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

		send(t, stream, acceptedFrame(other, 1))
		h.expectCloseCode(stream, codes.InvalidArgument)
	})

	t.Run("run_id does not exist", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())

		stream := h.openStream()
		send(t, stream, helloFrame("zzzzzzzzzzzz", agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.expectCloseCode(stream, codes.NotFound)
	})

	t.Run("run ended", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)
		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.expectCloseCode(stream, codes.FailedPrecondition)
	})

	t.Run("run failed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		h.provider.FailNext(fakeprovider.OpProvision, errors.New("provision refused"))
		runID := h.createRun("first")
		h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.expectCloseCode(stream, codes.FailedPrecondition)
	})
}

// TestProtocolAppendOnClosingStream appends while the registered stream is
// closing. A closed stream is a disconnected run, not a failed write: the
// message must be stored, AppendInbox must succeed, and the next Hello must
// deliver it. Regression test for the race the stress test first exposed.
func TestProtocolAppendOnClosingStream(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _, cancel := stepToBoundaryCancelable(t, h)

	gate := newLockGate("append")
	h.useGate(t, gate)

	appendDone := make(chan appendResult, 1)
	go func() {
		seq, err := h.ctrl.AppendInbox(context.Background(), runID, "second")
		appendDone <- appendResult{seq: seq, err: err}
	}()
	gate.waitEntered(t)

	// Close the stream while the append holds the row lock. The stream is
	// marked closed before its onStreamClosed transaction can acquire the
	// lock the append still holds.
	cancel()
	awaitLockWaiter(t, h.pool)

	gate.open()
	res := <-appendDone
	if res.err != nil {
		t.Fatalf("流关闭时 AppendInbox 失败: %v", res.err)
	}
	if res.seq != 2 {
		t.Fatalf("inbox_seq = %d, 想要 2", res.seq)
	}

	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	var delivered int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT delivered_through FROM run WHERE run_id = $1`, runID).Scan(&delivered); err != nil {
		t.Fatalf("查询 delivered_through: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered_through = %d, 想要 1", delivered)
	}

	// A new stream's Hello sees the stored but undelivered message and
	// delivers it.
	stream2 := h.openStream()
	rec := recordControl(stream2)
	send(t, stream2, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
	eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
	eventually(t, "新流收到消息 2", func() bool { return rec.countDeliverSeq(2) > 0 })
	if got := rec.countDeliverSeq(2); got != 1 {
		t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要 1 次", got)
	}
}

// TestProtocolReplacement covers “同一运行实例的第二条流取代第一条”: the
// first stream closes with Aborted and frames already in flight on it do not
// change the run after the replacement.
func TestProtocolReplacement(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream1 := h.openStream()
	send(t, stream1, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

	gate := newLockGate("hello")
	h.useGate(t, gate)

	stream2 := h.openStream()
	rec := recordControl(stream2)
	helloDone := make(chan error, 1)
	go func() {
		helloDone <- stream2.Send(helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	}()
	gate.waitEntered(t)

	// The first stream is still the registered stream at this point, so the
	// accepted frame is processed and waits behind the second Hello's row lock.
	acceptedDone := make(chan error, 1)
	go func() { acceptedDone <- stream1.Send(acceptedFrame(runID, 1)) }()
	awaitLockWaiter(t, h.pool)

	gate.open()
	if err := <-helloDone; err != nil {
		t.Fatalf("第二条流发送 Hello: %v", err)
	}
	if err := <-acceptedDone; err != nil {
		t.Fatalf("第一条流发送 Accepted: %v", err)
	}

	h.expectCloseCode(stream1, codes.Aborted)
	eventually(t, "第二条流收到 HelloAck", rec.helloAckSeen)
	eventually(t, "第二条流收到 Deliver", func() bool { return rec.countDeliverSeq(1) > 0 })
	awaitNoLockWaiter(t, h.pool)
	rowLockBarrier(t, h.ctrl, runID)

	state, substate := h.state(runID)
	if state != runcontroller.StateActive || substate != runcontroller.SubstateDelivering {
		t.Fatalf("旧流上的帧改变了状态: %s/%s, 想要 active/delivering", state, substate)
	}

	var delivered int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT delivered_through FROM run WHERE run_id = $1`, runID).Scan(&delivered); err != nil {
		t.Fatalf("查询 delivered_through: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("旧流上的 Accepted 生效了: delivered_through = %d, 想要 0", delivered)
	}
	if got := rec.countDeliverSeq(1); got != 1 {
		t.Fatalf("第二条流上消息 1 的 Deliver = %d 次, 想要 1 次", got)
	}
}

// TestProtocolHelloDoesNotRegressPosition sends a Hello whose
// last_accepted_seq is below delivered_through. Hello must keep the larger
// position instead of moving it backwards and re-delivering the message.
func TestProtocolHelloDoesNotRegressPosition(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream1 := h.openStream()
	send(t, stream1, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
	send(t, stream1, acceptedFrame(runID, 1))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
	send(t, stream1, turnStartedFrame(runID, "turn-1", "delivery-1"))
	send(t, stream1, turnEndedFrame(runID, "turn-1"))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	stream2 := h.openStream()
	rec := recordControl(stream2)
	send(t, stream2, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 0, nil))
	eventually(t, "第二条流收到 HelloAck", rec.helloAckSeen)

	// 取行锁屏障让 Hello 事务先提交；随后的 End 把 Shutdown 排在 Hello 事务
	// 预留的任何帧之后，收到 Shutdown 就证明错误的 Deliver 也已经到达，
	// 不用真实睡眠。
	rowLockBarrier(t, h.ctrl, runID)
	if err := h.ctrl.End(context.Background(), runID); err != nil {
		t.Fatalf("End: %v", err)
	}
	eventually(t, "第二条流收到 Shutdown", rec.shutdownSeen)

	var delivered int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT delivered_through FROM run WHERE run_id = $1`, runID).Scan(&delivered); err != nil {
		t.Fatalf("查询 delivered_through: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("Hello 把位置回退了: delivered_through = %d, 想要 1", delivered)
	}
	if got := rec.countDeliverSeq(1); got != 0 {
		t.Fatalf("位置回退导致消息 1 重复投递 %d 次", got)
	}
}

// TestProtocolDropAcceptedReconnect drops Accepted, disconnects and reconnects.
// The next Hello's last_accepted_seq recovers the position and the turn
// result, and the message is written exactly once.
func TestProtocolDropAcceptedReconnect(t *testing.T) {
	t.Parallel()

	h := newE2E(t, e2eConfig())
	h.setWardenInit(func(w *fakewarden.Warden) { w.DropNext(fakewarden.DropAccepted) })

	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitWritten(w, 1)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

	eventually(t, "假看守进程结束回合", func() bool {
		turns := w.Turns()
		return len(turns) == 1 && turns[0].Ended
	})

	connects := w.Connects()
	w.Disconnect()
	eventually(t, "假看守进程重连", func() bool { return w.Connects() > connects })

	// Hello 的 last_accepted_seq 恢复了位置：库内 delivered_through 变成 1，
	// 新流上不再重投消息 1。
	eventually(t, "delivered_through 恢复为 1", func() bool {
		var delivered int64
		if err := h.pool.QueryRow(context.Background(),
			`SELECT delivered_through FROM run WHERE run_id = $1`, runID).Scan(&delivered); err != nil {
			return false
		}
		return delivered == 1
	})
	assertNoDeliverOnCurrentStream(t, w, 1)
	if got := w.DeliverCounts()[1]; got != 1 {
		t.Fatalf("消息 1 的 Deliver 次数 = %d, 想要 1 次（Hello 只恢复位置，不重投）", got)
	}

	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	// The close transaction and the reconnect Hello both move the run; wait
	// for the turn result the Hello carries instead of reading it once.
	eventually(t, "重连后回合结果", func() bool {
		detail, err := h.detail(runID)
		return err == nil && len(detail.Turns) == 1 && detail.Turns[0].EndedAt != nil
	})
	assertWritten(t, w.Written(), 1)
}

// TestProtocolDropDeliverReconnect drops a whole Deliver, disconnects and
// reconnects. The next Hello reports the lower position, the control plane
// delivers again, and the message is written exactly once.
func TestProtocolDropDeliverReconnect(t *testing.T) {
	t.Parallel()

	h := newE2E(t, e2eConfig())
	h.setWardenInit(func(w *fakewarden.Warden) { w.DropNext(fakewarden.DropDeliver) })

	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
	if got := len(w.Written()); got != 0 {
		t.Fatalf("Deliver 被丢弃后 warden 写入了 %d 条消息", got)
	}

	w.Disconnect()
	h.waitWritten(w, 1)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	eventually(t, "重连后回合结果", func() bool {
		detail, err := h.detail(runID)
		return err == nil && len(detail.Turns) == 1 && detail.Turns[0].EndedAt != nil
	})
	assertWritten(t, w.Written(), 1)
	if got := w.DeliverCounts()[1]; got != 2 {
		t.Fatalf("消息 1 的 Deliver 次数 = %d, 想要 2 次（第一次被丢弃，重连后重投）", got)
	}
}

// TestProtocolDropTurnEndedReconnect drops turn_ended, disconnects and
// reconnects. Hello.last_turn completes the turn result, and the message is
// written exactly once.
func TestProtocolDropTurnEndedReconnect(t *testing.T) {
	t.Parallel()

	h := newE2E(t, e2eConfig())
	h.setWardenInit(func(w *fakewarden.Warden) { w.DropNext(fakewarden.DropTurnEnded) })

	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitWritten(w, 1)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	eventually(t, "假看守进程结束回合", func() bool {
		turns := w.Turns()
		return len(turns) == 1 && turns[0].Ended
	})
	eventually(t, "控制面记录回合开始", func() bool {
		detail, err := h.detail(runID)
		return err == nil && len(detail.Turns) == 1
	})
	detail, err := h.detail(runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Turns[0].EndedAt != nil {
		t.Fatalf("turn_ended 被丢弃后回合 = %+v, 想要 1 个未结束的回合", detail.Turns)
	}

	w.Disconnect()
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	eventually(t, "重连后回合结果", func() bool {
		detail, err := h.detail(runID)
		return err == nil && len(detail.Turns) == 1 && detail.Turns[0].EndedAt != nil
	})
	assertWritten(t, w.Written(), 1)
}
