package runcontroller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// TestReviewFixDeliverWaitsForCommit holds a Deliver reservation and then makes
// the transaction commit fail. The warden must not receive that Deliver, and
// the next message must still be written exactly once.
func TestReviewFixDeliverWaitsForCommit(t *testing.T) {
	t.Parallel()

	h := newE2E(t, testConfig())
	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	installFailCommitTrigger(t, h.pool, runID)
	if _, err := h.appendInbox(runID, "second"); err == nil {
		t.Fatal("提交失败时 AppendInbox 返回 nil")
	}
	dropFailCommitTrigger(context.Background(), h.pool)

	// The rolled-back Deliver must never reach the warden. The send (if it
	// happened at all) is asynchronous, so give it a moment.
	time.Sleep(250 * time.Millisecond)
	if got := w.Written(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("提交失败的 Deliver 被写入: Written = %v, 想要 [1]", got)
	}

	if _, err := h.appendInbox(runID, "third"); err != nil {
		t.Fatalf("AppendInbox(third): %v", err)
	}
	h.waitWritten(w, 2)

	got := w.Written()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Written = %v, 想要 [1 2]", got)
	}

	var text string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT text FROM inbox WHERE run_id = $1 AND inbox_seq = 2`, runID).Scan(&text); err != nil {
		t.Fatalf("查询 inbox 第 2 条: %v", err)
	}
	if text != "third" {
		t.Fatalf("inbox 第 2 条 = %q, 想要 third", text)
	}
}

// TestReviewFixCommitSucceededDespiteError makes Commit return an error after
// it actually succeeded, and advances the run with another transaction between
// that error and the outcome check. The outcome must be judged by the
// transaction status, so the write counts as committed and the message is
// delivered exactly once, even though the version changed in between.
func TestReviewFixCommitSucceededDespiteError(t *testing.T) {
	t.Parallel()

	h := newE2E(t, testConfig())
	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)
	eventually(t, "provision_returned_at", func() bool {
		var at *time.Time
		if err := h.pool.QueryRow(context.Background(),
			`SELECT provision_returned_at FROM run WHERE run_id = $1`, runID).Scan(&at); err != nil {
			return false
		}
		return at != nil
	})

	var once sync.Once
	runcontroller.SetCommitErrorAfterSuccess(h.ctrl, func() error {
		var err error
		once.Do(func() { err = errors.New("test: commit succeeded but returned an error") })
		return err
	})

	var advanced sync.Once
	runcontroller.SetAfterCommitErrorHook(h.ctrl, func() {
		advanced.Do(func() {
			// Another transaction advances the same run after the error and
			// before the outcome is judged. It changes the row version, which
			// makes the version comparison ambiguous.
			if _, err := appendHuman(h.ctrl, context.Background(), runID, "advance"); err != nil {
				t.Errorf("插入另一个事务: %v", err)
			}
		})
	})

	seq, err := appendHuman(h.ctrl, context.Background(), runID, "second")
	if err != nil {
		t.Fatalf("提交实际成功时 AppendInbox = %v, 想要 nil", err)
	}
	if seq != 2 {
		t.Fatalf("inbox_seq = %d, 想要 2", seq)
	}

	// The Deliver released by the committed transaction reaches the warden,
	// and the message is written exactly once.
	h.waitWritten(w, 2)
	h.waitWritten(w, 3)
	written := w.Written()
	seen := make(map[int64]int, len(written))
	for _, s := range written {
		seen[s]++
	}
	if seen[2] != 1 {
		t.Fatalf("第 2 条消息写入 %d 次，Written = %v，想要恰好一次", seen[2], written)
	}
	want := []int64{1, 2, 3}
	if fmt.Sprint(written) != fmt.Sprint(want) {
		t.Fatalf("Written = %v, 想要 %v", written, want)
	}
}

// TestReviewFixCommitAbortedDespiteMatchingVersion makes Commit actually roll
// back, then writes the same row version from another transaction before the
// outcome is judged. The outcome must be judged by the transaction status, so
// the write counts as rolled back even though the version matches.
func TestReviewFixCommitAbortedDespiteMatchingVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()

	var version int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT version FROM run WHERE run_id = $1`, runID).Scan(&version); err != nil {
		t.Fatalf("读取 version: %v", err)
	}

	// The rule transaction writes substate delivering; make only that update
	// fail at commit, so the interleaving update below can run.
	installFailCommitTriggerWhen(t, h.pool, runID, "NEW.substate = 'delivering'")

	var once sync.Once
	runcontroller.SetAfterCommitErrorHook(h.ctrl, func() {
		once.Do(func() {
			// Write the same version the rolled-back transaction wrote.
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE run SET version = $1 WHERE run_id = $2`, version+1, runID); err != nil {
				t.Errorf("写入相同 version: %v", err)
			}
		})
	})

	if _, err := appendHuman(h.ctrl, context.Background(), runID, "second"); err == nil {
		t.Fatal("提交实际回滚时 AppendInbox = nil, 想要错误")
	}

	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox WHERE run_id = $1 AND inbox_seq = 2`, runID).Scan(&count); err != nil {
		t.Fatalf("查询 inbox: %v", err)
	}
	if count != 0 {
		t.Fatalf("回滚的消息写入了数据库 %d 次", count)
	}
	if state, substate := h.state(runID); state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("状态 = %s/%s, 想要 active/boundary", state, substate)
	}
}

// TestReviewFixCommitUnknownClosesStreamAndAbandonsCall makes every
// pg_xact_status query fail, so the outcome retries run out. The rule must
// close the current stream, abandon the provider call it reserved, and return
// an error.
func TestReviewFixCommitUnknownClosesStreamAndAbandonsCall(t *testing.T) {
	t.Parallel()

	h := newE2E(t, testConfig())
	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	var once sync.Once
	runcontroller.SetCommitErrorAfterSuccess(h.ctrl, func() error {
		var err error
		once.Do(func() { err = errors.New("test: commit outcome unknown") })
		return err
	})
	runcontroller.SetCommitStatusError(h.ctrl, func() error {
		return errors.New("test: pg_xact_status is unavailable")
	})

	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err == nil {
		t.Fatal("提交结果无法判定时 TrySuspendNow 返回 nil, 想要错误")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := w.WaitFor(ctx, func() bool { return w.LastCloseCode() == codes.Unavailable }); err != nil {
		t.Fatalf("提交结果无法判定时当前流没有关闭: 关闭码 = %v, 想要 Unavailable (%v)",
			w.LastCloseCode(), err)
	}

	// The reserved Snapshot must be abandoned, not executed after the close.
	done := make(chan struct{})
	go func() {
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("预留的提供者调用没有被放弃，调用队列被阻塞")
	}
	if n := h.callCount(fakeprovider.OpSnapshot); n != 0 {
		t.Fatalf("提交结果无法判定时仍执行了 %d 次 Snapshot", n)
	}
}

// TestReviewFixApplyEventPanicReleasesReservations makes a rule panic after its
// transaction committed. The deferred cleanup must cancel the reserved frame
// and abandon the reserved provider call, so later events and the provider
// queue are not blocked behind a position that will never be resolved.
func TestReviewFixApplyEventPanicReleasesReservations(t *testing.T) {
	t.Parallel()

	t.Run("reserved frame", func(t *testing.T) {
		t.Parallel()

		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		var once sync.Once
		runcontroller.SetAfterCommitHook(h.ctrl, func(op, _ string) {
			if op == "append" {
				once.Do(func() { panic("test: panic after commit") })
			}
		})

		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("applyEvent 没有把 panic 传出去")
				}
			}()
			_, _ = appendHuman(h.ctrl, context.Background(), runID, "second")
		}()

		// A later End reserves a Shutdown behind the panicked transaction's
		// Deliver. If that position were never resolved, pump would wait
		// forever and the Shutdown would never arrive.
		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := w.WaitFor(ctx, func() bool { return w.ShutdownReceived() }); err != nil {
			t.Fatalf("panic 后预留的帧没有撤销，Shutdown 没有到达: %v", err)
		}
	})

	t.Run("reserved provider call", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		var once sync.Once
		h.setAfterCommitHook(func(op, _ string) {
			if op == "try_suspend" {
				once.Do(func() { panic("test: panic after commit") })
			}
		})

		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("applyEvent 没有把 panic 传出去")
				}
			}()
			_ = runcontroller.TrySuspendNow(h.ctrl, runID)
		}()

		if n := h.callCount(fakeprovider.OpSnapshot); n != 0 {
			t.Fatalf("panic 后仍执行了 %d 次 Snapshot", n)
		}
		// A barrier at the end of the queue must not wait for the abandoned
		// reservation.
		done := make(chan struct{})
		go func() {
			runcontroller.WaitProviderQueue(h.ctrl, runID)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("panic 后预留的提供者调用没有被放弃，调用队列被阻塞")
		}
	})
}

// TestReviewFixProvisionReturnTimeSurvivesRetry fails the first write of the
// Provision completion and advances the clock before the retry. The stored
// provision_returned_at must be the provider call's actual return time, not
// the retry time, because T_hello starts there.
func TestReviewFixProvisionReturnTimeSurvivesRetry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	want := h.clock.Now()

	var once sync.Once
	h.setHook(func(op, _ string) {
		if op != "provision_done" {
			return
		}
		once.Do(func() {
			terminateIdleRunTransaction(t, h.pool)
			h.clock.Advance(45 * time.Second)
		})
	})

	runID := h.createRun("first")
	h.waitProvisionReturned(runID)

	var got time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT provision_returned_at FROM run WHERE run_id = $1`, runID).Scan(&got); err != nil {
		t.Fatalf("读取 provision_returned_at: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("provision_returned_at = %v, 想要提供者返回时刻 %v（重试时的时钟是 %v）",
			got, want, h.clock.Now())
	}
}

// TestReviewFixRestoreReturnTimeSurvivesRetry is the RestoreSnapshot version of
// the Provision test above.
func TestReviewFixRestoreReturnTimeSurvivesRetry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)
	want := h.clock.Now()

	var once sync.Once
	h.setHook(func(op, _ string) {
		if op != "restore_done" {
			return
		}
		once.Do(func() {
			terminateIdleRunTransaction(t, h.pool)
			h.clock.Advance(45 * time.Second)
		})
	})

	if _, err := appendHuman(h.ctrl, context.Background(), runID, "wake"); err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	h.waitRestoreReturned(runID)

	var got time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT restore_returned_at FROM suspension WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
		runID).Scan(&got); err != nil {
		t.Fatalf("读取 restore_returned_at: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("restore_returned_at = %v, 想要提供者返回时刻 %v（重试时的时钟是 %v）",
			got, want, h.clock.Now())
	}
}

// TestReviewFixStreamCloseRetryIsBoundedWithoutRun closes a stream while Run
// is not started and the database rejects writes. The close write retries must
// end, not loop forever on a nil Run context.
func TestReviewFixStreamCloseRetryIsBoundedWithoutRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	runcontroller.SetStreamCloseRetryLimit(h.ctrl, 2)

	// Every write now fails; the pool is closed on purpose.
	h.pool.Close()

	done := make(chan struct{})
	go func() {
		runcontroller.StreamClosedForTest(h.ctrl, runID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run 未启动时，流关闭事件的重试没有有界结束")
	}
}

// TestReviewFixEndIgnoresClosedStreamShutdown ends a run while its stream is
// already closed but still registered. The Shutdown frame cannot be sent; End
// must still commit and return nil.
func TestReviewFixEndIgnoresClosedStreamShutdown(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	runcontroller.RegisterClosedStreamForTest(h.ctrl, runID)

	if err := h.ctrl.End(context.Background(), runID); err != nil {
		t.Fatalf("End 在流已关闭时返回错误: %v", err)
	}
	h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
}

// TestReviewFixCallerContextCancelDuringCommit cancels the caller context while
// the rule transaction is running. The transaction must commit anyway and the
// resulting state must match the provider call it scheduled.
func TestReviewFixCallerContextCancelDuringCommit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.suspendedRun()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.setHook(func(op, _ string) {
		if op == "append" {
			cancel()
		}
	})

	seq, err := appendHuman(h.ctrl, ctx, runID, "wake")
	if err != nil {
		t.Fatalf("调用方 ctx 取消后 AppendInbox = %v, 想要 nil", err)
	}
	if seq != 2 {
		t.Fatalf("inbox_seq = %d, 想要 2", seq)
	}

	h.waitCalls(fakeprovider.OpRestore, 1)
	h.waitRestoreReturned(runID)
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
}

// TestReviewFixCompletionRetry terminates the transaction backend while the
// snapshot completion event is being written. The write must be retried, and
// the run must leave suspending.
func TestReviewFixCompletionRetry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := h.stepToBoundary()
	h.clock.Advance(testConfig().IdleGrace + time.Second)

	var once sync.Once
	h.setHook(func(op, _ string) {
		if op != "snapshot_done" {
			return
		}
		once.Do(func() { terminateIdleRunTransaction(t, h.pool) })
	})

	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitCalls(fakeprovider.OpSnapshot, 1)
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)
}

// TestReviewFixCreateWithCancelledContext calls Create with an already
// cancelled context. It must return an error, not panic on a nil slot.
func TestReviewFixCreateWithCancelledContext(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.ctrl.Create(ctx, "prompt"); err == nil {
		t.Fatal("已取消的 ctx 调 Create 返回 nil, 想要错误")
	}
}

// TestReviewFixHelloExitedClosesNewStream freezes the warden, makes the agent
// exit, and reconnects. The Hello carries phase exited and must close the
// stream that sent it, failing the run.
func TestReviewFixHelloExitedClosesNewStream(t *testing.T) {
	t.Parallel()

	h := newE2E(t, testConfig())
	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	w.Freeze()
	w.Exit()
	w.Thaw()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := w.WaitFor(ctx, func() bool { return w.LastCloseCode() == codes.FailedPrecondition }); err != nil {
		t.Fatalf("重连后流的关闭码 = %v, 想要 FailedPrecondition", w.LastCloseCode())
	}
	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	if !strings.Contains(detail.FailureReason, "agent exited") {
		t.Fatalf("FailureReason = %q, 想要包含 agent exited", detail.FailureReason)
	}
}

// TestReviewFixAcceptedThroughSeq checks that Accepted must carry the through
// sequence of the last Deliver on that stream.
func TestReviewFixAcceptedThroughSeq(t *testing.T) {
	t.Parallel()

	t.Run("mismatched through_seq closes the stream", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)
		if _, err := appendHuman(h.ctrl, context.Background(), runID, "second"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		waitForDeliverThrough(t, stream, 2)

		// 1 is inside [1, allocated] but is not the last Deliver's through_seq.
		send(t, stream, acceptedFrame(runID, 1))
		h.expectCloseCode(stream, codes.InvalidArgument)
	})

	t.Run("matching through_seq is accepted", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, testConfig())
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)
		if _, err := appendHuman(h.ctrl, context.Background(), runID, "second"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}

		stream := h.openStream()
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		waitForDeliverThrough(t, stream, 2)
		send(t, stream, acceptedFrame(runID, 2))

		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
	})
}

// waitForDeliverThrough reads control frames until a Deliver with the given
// through sequence arrives.
func waitForDeliverThrough(t *testing.T, stream agentprotocolv1.AgentStream_ConnectClient, through int64) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if d := f.GetDeliver(); d != nil && d.GetThroughSeq() == through {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("等待 Deliver through_seq=%d: %v", through, err)
		}
	case <-time.After(testTimeout):
		t.Fatalf("等待 Deliver through_seq=%d 超时", through)
	}
}

// installFailCommitTrigger installs a deferred constraint trigger that makes
// any commit writing the run row fail.
func installFailCommitTrigger(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION rc_fail_commit() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'rc_fail_commit: forced commit failure';
			RETURN NEW;
		END $$`); err != nil {
		t.Fatalf("创建失败触发器函数: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER rc_fail_commit
		AFTER UPDATE ON run
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW
		WHEN (NEW.run_id = %s)
		EXECUTE FUNCTION rc_fail_commit()`, quoteLiteral(runID))); err != nil {
		t.Fatalf("创建失败触发器: %v", err)
	}
	t.Cleanup(func() { dropFailCommitTrigger(context.Background(), pool) })
}

// installFailCommitTriggerWhen is installFailCommitTrigger with an extra SQL
// condition on NEW, so only updates matching it abort at commit.
func installFailCommitTriggerWhen(t *testing.T, pool *pgxpool.Pool, runID, condition string) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION rc_fail_commit() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'rc_fail_commit: forced commit failure';
			RETURN NEW;
		END $$`); err != nil {
		t.Fatalf("创建失败触发器函数: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER rc_fail_commit
		AFTER UPDATE ON run
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW
		WHEN (NEW.run_id = %s AND %s)
		EXECUTE FUNCTION rc_fail_commit()`, quoteLiteral(runID), condition)); err != nil {
		t.Fatalf("创建失败触发器: %v", err)
	}
	t.Cleanup(func() { dropFailCommitTrigger(context.Background(), pool) })
}

func dropFailCommitTrigger(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS rc_fail_commit ON run`)
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS rc_fail_commit()`)
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// terminateIdleRunTransaction terminates the backend of the one rule
// transaction that is idle in transaction after taking its row lock.
func terminateIdleRunTransaction(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Errorf("打开独立连接: %v", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()

	deadline := time.Now().Add(testTimeout)
	for {
		var pid int
		err := conn.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'idle in transaction'
			  AND query ILIKE '%FOR UPDATE%'
			  AND pid <> pg_backend_pid()
			LIMIT 1`).Scan(&pid)
		if err == nil {
			var terminated bool
			if err := conn.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
				t.Errorf("pg_terminate_backend: %v", err)
			} else if !terminated {
				t.Errorf("pg_terminate_backend(%d) = false", pid)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("没有找到处于 idle in transaction 的规则事务")
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
