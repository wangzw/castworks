package runcontroller_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// lockGate blocks the after-lock hook of one event until open is called.
type lockGate struct {
	op      string
	entered chan struct{}
	release chan struct{}

	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newLockGate(op string) *lockGate {
	return &lockGate{
		op:      op,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *lockGate) hook(op, _ string) {
	if op != g.op {
		return
	}
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.release
}

func (g *lockGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(testTimeout):
		t.Fatalf("事件 %q 没有走到取得行锁之后", g.op)
	}
}

func (g *lockGate) open() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// useGate installs g as the after-lock hook and guarantees it is released
// during cleanup so a failed test cannot leave a transaction holding a lock.
func (h *harness) useGate(t *testing.T, g *lockGate) {
	t.Helper()
	h.setHook(g.hook)
	t.Cleanup(func() {
		g.open()
		h.setHook(nil)
	})
}

// suspendedRun creates an active/boundary run and suspends it straight away.
func (h *harness) suspendedRun() string {
	h.t.Helper()
	runID, _ := h.stepToBoundary()
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		h.t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)
	return runID
}

// resumingRun suspends a run, writes a message and waits for RestoreSnapshot
// to return, leaving it in resuming and waiting for Hello.
func (h *harness) resumingRun() string {
	h.t.Helper()
	runID := h.suspendedRun()
	if _, err := h.ctrl.AppendInbox(context.Background(), runID, "wake"); err != nil {
		h.t.Fatalf("AppendInbox: %v", err)
	}
	h.waitRestoreReturned(runID)
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
	return runID
}

func (h *harness) expectCloseCode(stream agentprotocolv1.AgentStream_ConnectClient, want codes.Code) {
	h.t.Helper()

	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if code := status.Code(err); code != want {
			h.t.Errorf("流关闭码 = %v, 想要 %v", code, want)
		}
	case <-time.After(testTimeout):
		h.t.Fatal("等待流关闭超时")
	}
}

func TestInterleaveAppendVsSuspend(t *testing.T) {
	t.Parallel()

	t.Run("message first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		gate := newLockGate("append")
		h.useGate(t, gate)

		var (
			seq int64
			err error
		)
		appendDone := make(chan struct{})
		go func() {
			seq, err = h.ctrl.AppendInbox(context.Background(), runID, "second")
			close(appendDone)
		}()
		gate.waitEntered(t)

		suspendDone := make(chan error, 1)
		go func() { suspendDone <- runcontroller.TrySuspendNow(h.ctrl, runID) }()
		h.waitForLockWaiter()

		gate.open()
		<-appendDone
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}
		if err := <-suspendDone; err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}

		// 消息先提交时运行实例已连接，AppendInbox 在同一个事务里投递它，
		// 子状态因此是 delivering 而不是 boundary。
		state, substate := h.state(runID)
		if state != runcontroller.StateActive || substate != runcontroller.SubstateDelivering {
			t.Fatalf("消息先提交后状态 = %s/%s, 想要 active/delivering（消息已投递）", state, substate)
		}
		if got := h.callCount(fakeprovider.OpSnapshot); got != 0 {
			t.Fatalf("消息先提交却调用了 %d 次 Snapshot", got)
		}
	})

	t.Run("suspend first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		releaseSnapshot := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(releaseSnapshot)

		gate := newLockGate("try_suspend")
		h.useGate(t, gate)

		suspendDone := make(chan error, 1)
		go func() { suspendDone <- runcontroller.TrySuspendNow(h.ctrl, runID) }()
		gate.waitEntered(t)

		appendDone := make(chan struct{})
		var (
			seq int64
			err error
		)
		go func() {
			seq, err = h.ctrl.AppendInbox(context.Background(), runID, "second")
			close(appendDone)
		}()
		h.waitForLockWaiter()

		gate.open()
		if err := <-suspendDone; err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		<-appendDone
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}

		// The message landed while the run was suspending; finishing the
		// snapshot must restore it.
		releaseSnapshot()
		h.waitCalls(fakeprovider.OpRestore, 1)
		h.waitRestoreReturned(runID)
		h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
	})
}

func TestInterleaveAppendVsSnapshotDone(t *testing.T) {
	t.Parallel()

	t.Run("append first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		releaseSnapshot := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(releaseSnapshot)

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitCalls(fakeprovider.OpSnapshot, 1)

		// Hold the append after it took the row lock, then let Snapshot return.
		// Its completion transaction must queue behind the append's lock.
		gate := newLockGate("append")
		h.useGate(t, gate)

		appendDone := make(chan struct{})
		var (
			seq int64
			err error
		)
		go func() {
			seq, err = h.ctrl.AppendInbox(context.Background(), runID, "during snapshot")
			close(appendDone)
		}()
		gate.waitEntered(t)

		releaseSnapshot()
		h.waitForLockWaiter()

		gate.open()
		<-appendDone
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}

		h.waitCalls(fakeprovider.OpRestore, 1)
		h.waitRestoreReturned(runID)
		h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)

		if got := h.callCount(fakeprovider.OpRestore); got != 1 {
			t.Fatalf("RestoreSnapshot 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("snapshot done first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		gate := newLockGate("snapshot_done")
		h.useGate(t, gate)

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		gate.waitEntered(t)

		appendDone := make(chan struct{})
		var (
			seq int64
			err error
		)
		go func() {
			seq, err = h.ctrl.AppendInbox(context.Background(), runID, "after snapshot")
			close(appendDone)
		}()
		h.waitForLockWaiter()

		gate.open()
		<-appendDone
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}

		h.waitCalls(fakeprovider.OpRestore, 1)
		h.waitRestoreReturned(runID)
		h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)

		if got := h.callCount(fakeprovider.OpRestore); got != 1 {
			t.Fatalf("RestoreSnapshot 调用 %d 次, 想要恰好 1 次", got)
		}
	})
}

func TestInterleaveTwoAppends(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.suspendedRun()

	gate := newLockGate("append")
	h.useGate(t, gate)

	type appendResult struct {
		seq int64
		err error
	}
	first := make(chan appendResult, 1)
	second := make(chan appendResult, 1)

	go func() {
		seq, err := h.ctrl.AppendInbox(context.Background(), runID, "one")
		first <- appendResult{seq: seq, err: err}
	}()
	gate.waitEntered(t)

	go func() {
		seq, err := h.ctrl.AppendInbox(context.Background(), runID, "two")
		second <- appendResult{seq: seq, err: err}
	}()
	h.waitForLockWaiter()

	gate.open()
	r1 := <-first
	r2 := <-second
	if r1.err != nil || r2.err != nil {
		t.Fatalf("AppendInbox 错误: %v / %v", r1.err, r2.err)
	}
	if r1.seq == r2.seq {
		t.Fatalf("两个 AppendInbox 分配了相同的序号 %d", r1.seq)
	}
	seqs := map[int64]bool{r1.seq: true, r2.seq: true}
	if !seqs[2] || !seqs[3] {
		t.Fatalf("序号 = %d, %d, 想要 2 与 3", r1.seq, r2.seq)
	}

	h.waitCalls(fakeprovider.OpRestore, 1)
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
	if got := h.callCount(fakeprovider.OpRestore); got != 1 {
		t.Fatalf("RestoreSnapshot 调用 %d 次, 想要恰好 1 次", got)
	}
}

// TestInterleaveDeliverReservedBeforeCommit 验证 Deliver 在事务内预留、提交后
// 才放行：把 AppendInbox 停在提交之后、放行之前，新流的 Hello 先送出这条已
// 提交的消息；放行后新流上消息 2 仍然只出现一次。若实现退回“提交后才按当前
// 流入队”，这里会看到第二次投递。
func TestInterleaveDeliverReservedBeforeCommit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()

	gate := newLockGate("append")
	h.setAfterCommitHook(gate.hook)
	t.Cleanup(func() {
		gate.open()
		h.setAfterCommitHook(nil)
	})

	appendDone := make(chan appendResult, 1)
	go func() {
		seq, err := h.ctrl.AppendInbox(context.Background(), runID, "second")
		appendDone <- appendResult{seq: seq, err: err}
	}()
	gate.waitEntered(t)

	// The append has committed, but its Deliver is still reserved on the old
	// stream. A new stream's Hello delivers the committed message.
	stream2 := h.openStream()
	rec := recordControl(stream2)
	send(t, stream2, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
	eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
	eventually(t, "新流收到消息 2", func() bool { return rec.countDeliverSeq(2) > 0 })

	gate.open()
	res := <-appendDone
	if res.err != nil {
		t.Fatalf("AppendInbox: %v", res.err)
	}
	if res.seq != 2 {
		t.Fatalf("inbox_seq = %d, 想要 2", res.seq)
	}

	// End 的 Shutdown 排在 append 放行的一切之后；看到它就能确定 append
	// 若多送了一帧，那一帧已经在同一条流上到达。
	if err := h.ctrl.End(context.Background(), runID); err != nil {
		t.Fatalf("End: %v", err)
	}
	eventually(t, "新流收到 Shutdown", rec.shutdownSeen)
	if got := rec.countDeliverSeq(2); got != 1 {
		t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
	}
}

func TestInterleaveEndVsProviderCall(t *testing.T) {
	t.Parallel()

	t.Run("provision", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		release := h.provider.Hold(fakeprovider.OpProvision)
		t.Cleanup(release)

		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

		release()
		h.waitCalls(fakeprovider.OpDestroy, 1)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		release := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(release)

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitCalls(fakeprovider.OpSnapshot, 1)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

		release()
		h.waitCalls(fakeprovider.OpDestroy, 1)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("provision first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		release := h.provider.Hold(fakeprovider.OpProvision)
		t.Cleanup(release)

		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)
		release()
		h.waitProvisionReturned(runID)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("restore", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.suspendedRun()

		release := h.provider.Hold(fakeprovider.OpRestore)
		t.Cleanup(release)

		if _, err := h.ctrl.AppendInbox(context.Background(), runID, "wake"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitCalls(fakeprovider.OpRestore, 1)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

		release()
		h.waitCalls(fakeprovider.OpDestroy, 1)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("snapshot first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		release := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(release)

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitCalls(fakeprovider.OpSnapshot, 1)
		release()
		h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("restore first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.suspendedRun()

		release := h.provider.Hold(fakeprovider.OpRestore)
		t.Cleanup(release)

		if _, err := h.ctrl.AppendInbox(context.Background(), runID, "wake"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitCalls(fakeprovider.OpRestore, 1)
		release()
		h.waitRestoreReturned(runID)
		h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要恰好 1 次", got)
		}
	})
}

func TestInterleaveHelloVsExpire(t *testing.T) {
	t.Parallel()

	t.Run("recovered hello first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.resumingRun()
		h.clock.Advance(testConfig().HelloTimeout + time.Second)

		gate := newLockGate("hello")
		h.useGate(t, gate)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
		gate.waitEntered(t)

		expireDone := make(chan error, 1)
		go func() { expireDone <- runcontroller.ExpireHelloNow(h.ctrl, runID) }()
		h.waitForLockWaiter()

		gate.open()
		if err := <-expireDone; err != nil {
			t.Fatalf("ExpireHelloNow: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
	})

	t.Run("recovered expire first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.resumingRun()
		h.clock.Advance(testConfig().HelloTimeout + time.Second)

		gate := newLockGate("hello_expire")
		h.useGate(t, gate)

		expireDone := make(chan error, 1)
		go func() { expireDone <- runcontroller.ExpireHelloNow(h.ctrl, runID) }()
		gate.waitEntered(t)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
		h.waitForLockWaiter()

		gate.open()
		if err := <-expireDone; err != nil {
			t.Fatalf("ExpireHelloNow: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "hello timeout") {
			t.Fatalf("FailureReason = %q, 想要 hello timeout", detail.FailureReason)
		}
		h.expectCloseCode(stream, codes.FailedPrecondition)
	})

	t.Run("provisioned hello first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitProvisionReturned(runID)
		h.clock.Advance(testConfig().HelloTimeout + time.Second)

		gate := newLockGate("hello")
		h.useGate(t, gate)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		gate.waitEntered(t)

		expireDone := make(chan error, 1)
		go func() { expireDone <- runcontroller.ExpireHelloNow(h.ctrl, runID) }()
		h.waitForLockWaiter()

		gate.open()
		if err := <-expireDone; err != nil {
			t.Fatalf("ExpireHelloNow: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
	})

	t.Run("provisioned expire first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitProvisionReturned(runID)
		h.clock.Advance(testConfig().HelloTimeout + time.Second)

		gate := newLockGate("hello_expire")
		h.useGate(t, gate)

		expireDone := make(chan error, 1)
		go func() { expireDone <- runcontroller.ExpireHelloNow(h.ctrl, runID) }()
		gate.waitEntered(t)

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.waitForLockWaiter()

		gate.open()
		if err := <-expireDone; err != nil {
			t.Fatalf("ExpireHelloNow: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "hello timeout") {
			t.Fatalf("FailureReason = %q, 想要 hello timeout", detail.FailureReason)
		}
		h.expectCloseCode(stream, codes.FailedPrecondition)
	})
}
