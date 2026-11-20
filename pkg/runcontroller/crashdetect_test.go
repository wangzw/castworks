package runcontroller_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// crashTestConfig returns a config with an explicit T_reconnect so tests can
// advance the fake clock past it.
func crashTestConfig() runcontroller.Config {
	cfg := testConfig()
	cfg.ReconnectTimeout = 30 * time.Second
	return cfg
}

// waitDisconnected waits until the controller has recorded the run's stream
// disconnect in memory.
func waitDisconnected(t *testing.T, h *harness, runID string) {
	t.Helper()
	eventually(t, "断线时刻记录", func() bool {
		_, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID)
		return ok
	})
}

// disconnectRun closes the run's current stream and waits for the record.
func disconnectRun(t *testing.T, h *harness, runID string, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	waitDisconnected(t, h, runID)
}

// reconnectRun opens a new stream, sends a boundary Hello and waits for the
// HelloAck, which means the control plane has cleared the disconnect record.
func reconnectRun(t *testing.T, h *harness, runID string) {
	t.Helper()
	stream, _ := openStreamWithCancel(t, h)
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
	eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
	eventually(t, "清除断线记录", func() bool {
		_, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID)
		return !ok
	})
}

// TestActorCrashDetectionFailsRun covers the main rule: after the outbound
// stream has been gone for longer than T_reconnect, a crashed actor turns the
// run failed within one more scan period. The actor is not deleted.
func TestActorCrashDetectionFailsRun(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)
	actor := sandboxprovider.ActorName(runID)
	h.provider.SetCrashed(actor)

	disconnectRun(t, h, runID, cancel)
	h.startRun()

	// At exactly T_reconnect the run is not yet a candidate and must not be
	// queried.
	h.advanceScan(cfg.ReconnectTimeout)
	runcontroller.WaitProviderQueue(h.ctrl, runID)
	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("T_reconnect 边界上的状态 = %s/%s, 想要 active/boundary", state, substate)
	}
	if got := h.callCount(fakeprovider.OpGetSandbox); got != 0 {
		t.Fatalf("T_reconnect 边界上的 GetSandbox = %d 次, 想要 0", got)
	}

	// One more scan rounds the cutoff past the disconnect time.
	h.advanceScan(cfg.ScanPeriod)
	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	wantReason := fmt.Sprintf("actor %s is crashed", actor)
	if detail.FailureReason != wantReason {
		t.Fatalf("failure_reason = %q, 想要 %q", detail.FailureReason, wantReason)
	}
	runcontroller.WaitProviderQueue(h.ctrl, runID)
	if got := h.callCount(fakeprovider.OpDestroy); got != 0 {
		t.Fatalf("转为失败时调用了 %d 次 Destroy, 想要 0", got)
	}
	status, err := h.provider.GetSandbox(context.Background(), actor)
	if err != nil || status.State != sandboxprovider.ActorStateCrashed {
		t.Fatalf("actor 应保留为 crashed: status=%+v err=%v", status, err)
	}
}

// TestActorCrashNotFoundFailsRun covers the ErrNotFound branch and its
// failure_reason wording.
func TestActorCrashNotFoundFailsRun(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)
	actor := sandboxprovider.ActorName(runID)

	disconnectRun(t, h, runID, cancel)
	// Destroy removes the actor; the fake provider then reports ErrNotFound.
	if err := h.provider.Destroy(context.Background(), actor); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h.startRun()

	h.advanceScan(cfg.ReconnectTimeout)
	h.advanceScan(cfg.ScanPeriod)
	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	wantReason := fmt.Sprintf("actor %s is not found", actor)
	if detail.FailureReason != wantReason {
		t.Fatalf("failure_reason = %q, 想要 %q", detail.FailureReason, wantReason)
	}
}

// TestActorCrashReconnectWithinTimeout covers the stream reconnecting before
// T_reconnect passes: the run is not a candidate, GetSandbox is not called and
// the state does not change.
func TestActorCrashReconnectWithinTimeout(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)

	disconnectRun(t, h, runID, cancel)
	h.startRun()
	reconnectRun(t, h, runID)

	h.advanceScan(cfg.ReconnectTimeout + cfg.ScanPeriod)
	runcontroller.WaitProviderQueue(h.ctrl, runID)
	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("重连后的状态 = %s/%s, 想要 active/boundary", state, substate)
	}
	if got := h.callCount(fakeprovider.OpGetSandbox); got != 0 {
		t.Fatalf("T_reconnect 内重连后 GetSandbox = %d 次, 想要 0", got)
	}
}

// TestActorCrashOtherResultsAreRetried covers a running actor and a GetSandbox
// error: the state stays active and the next scan asks again.
func TestActorCrashOtherResultsAreRetried(t *testing.T) {
	t.Parallel()

	t.Run("running", func(t *testing.T) {
		t.Parallel()
		cfg := crashTestConfig()
		h := newHarness(t, cfg)
		runID, _, cancel := stepToBoundaryCancelable(t, h)

		disconnectRun(t, h, runID, cancel)
		h.startRun()

		h.advanceScan(cfg.ReconnectTimeout)
		h.advanceScan(cfg.ScanPeriod)
		h.waitCalls(fakeprovider.OpGetSandbox, 1)
		if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
			t.Fatalf("actor 运行中时的状态 = %s/%s, 想要 active/boundary", state, substate)
		}

		h.provider.SetCrashed(sandboxprovider.ActorName(runID))
		h.advanceScan(cfg.ScanPeriod)
		h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	})

	t.Run("get sandbox error", func(t *testing.T) {
		t.Parallel()
		cfg := crashTestConfig()
		h := newHarness(t, cfg)
		runID, _, cancel := stepToBoundaryCancelable(t, h)
		actor := sandboxprovider.ActorName(runID)

		disconnectRun(t, h, runID, cancel)
		h.provider.FailNext(fakeprovider.OpGetSandbox, errors.New("get sandbox failed"))
		h.startRun()

		h.advanceScan(cfg.ReconnectTimeout)
		h.advanceScan(cfg.ScanPeriod)
		h.waitCalls(fakeprovider.OpGetSandbox, 1)
		if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
			t.Fatalf("GetSandbox 出错后的状态 = %s/%s, 想要 active/boundary", state, substate)
		}

		h.provider.SetCrashed(actor)
		h.advanceScan(cfg.ScanPeriod)
		h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	})
}

// TestActorCrashRecheckReconnect covers a reconnect that lands after the scan
// has already selected the run and while GetSandbox is in flight: the
// transaction recheck sees the new stream and leaves the run unchanged.
func TestActorCrashRecheckReconnect(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)
	h.provider.SetCrashed(sandboxprovider.ActorName(runID))

	disconnectRun(t, h, runID, cancel)
	h.startRun()

	release := h.provider.Hold(fakeprovider.OpGetSandbox)
	defer release()

	h.advanceScan(cfg.ReconnectTimeout)
	h.advanceScan(cfg.ScanPeriod)
	h.waitCalls(fakeprovider.OpGetSandbox, 1)

	// The reconnect happens while the selected check is blocked in GetSandbox.
	reconnectRun(t, h, runID)
	release()
	runcontroller.WaitProviderQueue(h.ctrl, runID)

	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("事务内复查时已重连，状态 = %s/%s, 想要 active/boundary", state, substate)
	}
	detail, err := h.detail(runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.FailureReason != "" {
		t.Fatalf("failure_reason = %q, 想要为空", detail.FailureReason)
	}
}

// TestActorCrashStartupSeedsActiveRuns simulates a control-plane restart: a
// fresh controller starts while the run is active and its warden never
// reconnects. Every active run is treated as disconnected at startup.
func TestActorCrashStartupSeedsActiveRuns(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)
	actor := sandboxprovider.ActorName(runID)
	h.provider.SetCrashed(actor)
	cancel()
	waitDisconnected(t, h, runID)

	// Move the clock well past the old disconnect so the seeded time is
	// distinguishable from it: after the restart the run must still wait a
	// full T_reconnect from the seed, not from the old disconnect.
	h.clock.Advance(2 * cfg.ReconnectTimeout)

	// A second controller on the same database has no in-memory stream or
	// disconnect record for the run, exactly like a process restart.
	restarted := runcontroller.New(cfg, h.pool, h.provider, h.clock)
	ctx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- restarted.Run(ctx) }()
	t.Cleanup(func() {
		cancelRun()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run 返回错误: %v", err)
			}
		case <-time.After(testTimeout):
			t.Errorf("重启后的 Run 没有在关停后返回")
		}
	})

	if err := h.clock.WaitForTimers(context.Background(), 1); err != nil {
		t.Fatalf("等待扫描计时器: %v", err)
	}

	// The seed time is the restart time, so the run is not a candidate until a
	// full T_reconnect has passed since the restart.
	h.advanceScan(cfg.ReconnectTimeout)
	runcontroller.WaitProviderQueue(restarted, runID)
	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("T_reconnect 边界上的状态 = %s/%s, 想要 active/boundary", state, substate)
	}
	if got := h.callCount(fakeprovider.OpGetSandbox); got != 0 {
		t.Fatalf("播种时刻 + T_reconnect 的 GetSandbox = %d 次, 想要 0", got)
	}

	h.advanceScan(cfg.ScanPeriod)
	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	wantReason := fmt.Sprintf("actor %s is crashed", actor)
	if detail.FailureReason != wantReason {
		t.Fatalf("failure_reason = %q, 想要 %q", detail.FailureReason, wantReason)
	}
}

// TestActorCrashAfterClosedStreamDelivery covers the interleaving where the
// stream has already closed when AppendInbox's Deliver reserves a frame: the
// reserve fails with errStreamClosed and the delivery path unregisters the
// conn. That path must record the disconnect as well, so the crash scan still
// selects the run and turns it failed.
func TestActorCrashAfterClosedStreamDelivery(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, _ := stepToBoundaryCancelable(t, h)
	actor := sandboxprovider.ActorName(runID)
	h.provider.SetCrashed(actor)

	// The actor has crashed, so the stream is closed, but its close handler has
	// not run yet. AppendInbox's Deliver then reserves on the closed stream and
	// takes the delivery path's unregister branch.
	runcontroller.RegisterClosedStreamForTest(h.ctrl, runID)
	if _, _, err := h.ctrl.AppendInbox(context.Background(), runID, runcontroller.InboxEvent{
		Kind: runcontroller.KindHumanMessage,
		Text: "message after the crash",
	}); err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	if _, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID); !ok {
		t.Fatal("投递路径摘除关闭的流后没有记录断线时刻")
	}

	h.startRun()
	h.advanceScan(cfg.ReconnectTimeout)
	h.advanceScan(cfg.ScanPeriod)
	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	wantReason := fmt.Sprintf("actor %s is crashed", actor)
	if detail.FailureReason != wantReason {
		t.Fatalf("failure_reason = %q, 想要 %q", detail.FailureReason, wantReason)
	}
}

// TestActorCrashStaleStreamCloseIsIgnored covers a stream replaced by a newer
// one whose close handler returns afterwards: the late close must not record a
// disconnect for the run, because the run is still connected through the new
// stream.
func TestActorCrashStaleStreamCloseIsIgnored(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, _ := stepToBoundaryCancelable(t, h)
	h.startRun()

	// Two successful Hellos: the second replaces the first. Only afterwards does
	// the first stream's close handler run, as a late handler return would.
	first := runcontroller.NewStreamForTest(h.ctrl, runID)
	runcontroller.NewStreamForTest(h.ctrl, runID)
	runcontroller.CloseStaleStreamForTest(h.ctrl, first)

	if _, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID); ok {
		t.Fatal("旧流晚关后记录了断线时刻，但新流仍在")
	}

	h.advanceScan(cfg.ReconnectTimeout)
	h.advanceScan(cfg.ScanPeriod)
	runcontroller.WaitProviderQueue(h.ctrl, runID)
	if got := h.callCount(fakeprovider.OpGetSandbox); got != 0 {
		t.Fatalf("旧流晚关后 GetSandbox = %d 次, 想要 0", got)
	}
	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("旧流晚关后的状态 = %s/%s, 想要 active/boundary", state, substate)
	}
}

// TestActorCrashDisconnectRecordDroppedForTerminatedRun covers the in-memory
// record of a run that has terminated: the scan drops it, so records do not
// accumulate for the lifetime of the process.
func TestActorCrashDisconnectRecordDroppedForTerminatedRun(t *testing.T) {
	t.Parallel()

	cfg := crashTestConfig()
	h := newHarness(t, cfg)
	runID, _, cancel := stepToBoundaryCancelable(t, h)
	h.startRun()
	disconnectRun(t, h, runID, cancel)

	if err := h.ctrl.End(context.Background(), runID); err != nil {
		t.Fatalf("End: %v", err)
	}
	h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

	h.advanceScan(cfg.ScanPeriod)
	runcontroller.WaitProviderQueue(h.ctrl, runID)
	if _, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID); ok {
		t.Fatal("运行实例转为终态后断线记录没有被清理")
	}
}
