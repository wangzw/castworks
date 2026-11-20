package runcontroller_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// TestStressManyRuns runs several runs in parallel. Each clock round advances
// the fake clock through a suspension scan; between rounds several goroutines
// append messages to every run, and another goroutine disconnects random fake
// wardens. At the end every message must have been written exactly once in
// order, every seq must have arrived in exactly one Deliver, the provider must
// have taken at least one snapshot, every run must have as many turns as inbox
// messages, and no run may be left in a transitional state.
//
// Appends run between the clock rounds rather than racing the whole loop, so
// the append/suspend/restore interleaving always happens.
//
// The scale and the number of rounds are fixed constants, not derived from the
// CPU count.
func TestStressManyRuns(t *testing.T) {
	t.Parallel()

	const (
		runs        = 3
		appenders   = 2
		perAppender = 4
		clockRounds = 10
		disconnects = 6
		settleLimit = 30 * time.Second
	)

	cfg := e2eConfig()
	// A short idle grace and a long hello timeout make the scan suspend often
	// without ever letting a fake-clock jump fail a run that is resuming.
	cfg.IdleGrace = 20 * time.Millisecond
	cfg.ScanPeriod = 5 * time.Millisecond
	cfg.HelloTimeout = 10 * time.Minute

	h := newE2E(t, cfg)
	h.startRun()

	runIDs := make([]string, runs)
	wardens := make(map[string]*fakewarden.Warden, runs)
	runLocks := make([]sync.Mutex, runs)
	for i := range runIDs {
		runID := h.createRun(fmt.Sprintf("run %d first", i))
		runIDs[i] = runID
		wardens[runID] = h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	t.Cleanup(closeStop)

	// Appender errors travel on a buffered channel: reporting them with
	// t.Errorf from a goroutine could run after a failing test has finished.
	appendErrs := make(chan error, runs*appenders*perAppender)

	disconnectDone := make(chan struct{})
	go func() {
		defer close(disconnectDone)
		rng := rand.New(rand.NewSource(4242))
		for range disconnects {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			wardens[runIDs[rng.Intn(runs)]].Disconnect()
		}
	}()

	clockDone := make(chan struct{})
	go func() {
		defer close(clockDone)
		for round := range clockRounds {
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			err := h.clock.WaitForTimers(ctx, 1)
			cancel()
			if err != nil {
				return
			}
			h.clock.Advance(cfg.IdleGrace + cfg.ScanPeriod + time.Millisecond)
			ctx, cancel = context.WithTimeout(context.Background(), testTimeout)
			err = h.clock.WaitForTimers(ctx, 1)
			cancel()
			if err != nil {
				return
			}
			select {
			case <-stop:
				return
			default:
			}

			// Append between clock rounds: the run may be suspended here, so
			// the append races the next suspension scan and the restore.
			if round >= perAppender {
				continue
			}
			var wg sync.WaitGroup
			for i, runID := range runIDs {
				for a := range appenders {
					wg.Add(1)
					go func(runID string, runIndex int, seed int64) {
						defer wg.Done()
						rng := rand.New(rand.NewSource(seed))
						select {
						case <-stop:
							return
						default:
						}
						time.Sleep(time.Duration(1+rng.Intn(3)) * time.Millisecond)

						// One undelivered message at a time on a run: wait
						// until the previous one has been written. Two
						// messages pending at a boundary would be delivered
						// as one batch and produce one turn for both.
						runLocks[runIndex].Lock()
						defer runLocks[runIndex].Unlock()
						before := len(wardens[runID].Written())
						if _, err := appendHuman(h.controller(), context.Background(), runID,
							fmt.Sprintf("%s-%d-%d", runID, round, a)); err != nil {
							appendErrs <- fmt.Errorf("AppendInbox(%s): %w", runID, err)
							return
						}
						deadline := time.Now().Add(settleLimit)
						for len(wardens[runID].Written()) == before {
							if time.Now().After(deadline) {
								appendErrs <- fmt.Errorf("AppendInbox(%s): 消息没有在期限内写入", runID)
								return
							}
							time.Sleep(time.Millisecond)
						}
					}(runID, i, int64(i*1000+a*10+round+1))
				}
			}
			wg.Wait()
		}
	}()

	<-clockDone
	closeStop()
	<-disconnectDone

	select {
	case err := <-appendErrs:
		t.Fatalf("%v", err)
	default:
	}

	expected := 1 + appenders*perAppender
	deadline := time.Now().Add(settleLimit)
	for _, runID := range runIDs {
		for {
			state, substate := h.state(runID)
			written := len(wardens[runID].Written())
			stable := (state == runcontroller.StateActive && substate == runcontroller.SubstateBoundary) ||
				(state == runcontroller.StateSuspended && substate == runcontroller.SubstateSnapshotted)
			if written == expected && stable {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("运行实例 %s 未收敛: 状态 %s/%s, 写入 %v, 想要 1..%d",
					runID, state, substate, wardens[runID].Written(), expected)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if got := h.callCount(fakeprovider.OpSnapshot); got == 0 {
		t.Fatal("压力测试没有触发任何 Snapshot")
	}

	for _, runID := range runIDs {
		assertWritten(t, wardens[runID].Written(), expected)
		assertDeliverCounts(t, wardens[runID], expected)
		detail, err := h.detail(runID)
		if err != nil {
			t.Fatalf("Get %s: %v", runID, err)
		}
		if len(detail.Turns) != expected {
			t.Fatalf("运行实例 %s 的回合数 = %d, 想要 %d（与收件箱条数相等）",
				runID, len(detail.Turns), expected)
		}
	}
}
