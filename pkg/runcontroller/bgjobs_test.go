package runcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

// sameJobs 比较两个后台作业集合，nil 与空集合等价。
func sameJobs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// waitBgJobs 等到数据库中的后台作业集合变为 want。
func (h *e2eHarness) waitBgJobs(runID string, want []string) {
	h.t.Helper()
	eventually(h.t, "后台作业集合", func() bool {
		var got []string
		err := h.pool.QueryRow(context.Background(),
			`SELECT bg_jobs FROM run WHERE run_id = $1`, runID).Scan(&got)
		if err != nil {
			h.t.Fatalf("查询 bg_jobs: %v", err)
		}
		return sameJobs(got, want)
	})
}

// assertAtBoundary 断言运行实例仍停在 active/boundary。
func (h *e2eHarness) assertAtBoundary(runID string) {
	h.t.Helper()
	state, substate := h.state(runID)
	if state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		h.t.Fatalf("状态 = %s/%s, 想要 active/boundary", state, substate)
	}
}

// TestE2EBgJobsPreventSuspension 覆盖“回合边界空闲超过 G_idle 而 bg_jobs 非空
// 时不挂起”，以及报告空集之后从当时起再空闲满 G_idle 才挂起。
func TestE2EBgJobsPreventSuspension(t *testing.T) {
	h := newE2E(t, e2eConfig())
	h.setWardenInit(func(w *fakewarden.Warden) {
		w.SetBgJobs([]string{"job-1"})
	})
	h.startRun()

	runID := h.createRun("hello")
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(h.warden(runID), 1)
	h.waitBgJobs(runID, []string{"job-1"})

	// 空闲超过 G_idle，但后台作业还在：扫描不能挂起。
	h.advanceScan(h.cfg.IdleGrace + time.Second)
	h.assertAtBoundary(runID)

	// 报告空集：从这一刻起重新计时。
	h.warden(runID).BgJobsChanged(nil)
	h.waitBgJobs(runID, nil)

	// 只过了半个 G_idle：仍不挂起。
	h.advanceScan(h.cfg.IdleGrace / 2)
	h.assertAtBoundary(runID)

	// 再空闲满剩余的 G_idle：挂起。
	h.advanceScan(h.cfg.IdleGrace/2 + time.Second)
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)
}

// TestE2EBgJobsReconcileOnReconnect 覆盖断线期间丢失 bg_jobs_changed 后，重连的
// Hello.bg_jobs 替换已保存的集合。
func TestE2EBgJobsReconcileOnReconnect(t *testing.T) {
	h := newE2E(t, e2eConfig())
	h.startRun()

	runID := h.createRun("hello")
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	w := h.warden(runID)

	w.BgJobsChanged([]string{"job-1"})
	h.waitBgJobs(runID, []string{"job-1"})

	// 集合变成空，但这条 bg_jobs_changed 被丢弃；重连的 Hello 带着空集。
	w.DropNext(fakewarden.DropBgJobsChanged)
	w.BgJobsChanged(nil)
	w.Disconnect()

	h.waitBgJobs(runID, nil)
	h.assertAtBoundary(runID)
}

// TestE2EExitedHelloKeepsBgJobs 覆盖 phase 为 exited 的 Hello 不更新集合。
func TestE2EExitedHelloKeepsBgJobs(t *testing.T) {
	h := newE2E(t, e2eConfig())
	h.startRun()

	runID := h.createRun("hello")
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	w := h.warden(runID)

	w.BgJobsChanged([]string{"job-a"})
	h.waitBgJobs(runID, []string{"job-a"})

	// 看守进程退出，但 turn_ended 被丢弃，控制面仍以为运行实例活着。
	w.DropNext(fakewarden.DropTurnEnded)
	w.Exit()

	// 重连的 Hello 上报 exited，并带一个不同的集合；它不能覆盖已保存的集合。
	w.SetBgJobs([]string{"job-b"})
	w.Disconnect()

	h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	h.waitBgJobs(runID, []string{"job-a"})
}
