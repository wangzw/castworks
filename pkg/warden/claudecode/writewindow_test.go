package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// assertTurnEventInvariant 检查每个 TurnEnded 之前都有同一非空 turn_id 的
// TurnStarted。
func assertTurnEventInvariant(t *testing.T, events []sinkEvent) {
	t.Helper()
	started := make(map[string]bool)
	for _, event := range events {
		switch event.Kind {
		case eventTurnStarted:
			if event.TurnID == "" {
				t.Fatalf("turn_started has an empty turn id: %+v", events)
			}
			started[event.TurnID] = true
		case eventTurnEnded:
			if event.TurnID == "" || !started[event.TurnID] {
				t.Fatalf("turn_ended %q has no prior turn_started: %+v", event.TurnID, events)
			}
		}
	}
}

// waitInTurn 等到驱动器进入回合且写 stdin 仍在进行。
func waitInTurn(t *testing.T, driver *Driver) {
	t.Helper()
	waitFor(t, func() bool {
		inTurn := false
		driver.Snapshot(func(state agent.State) { inTurn = state.Phase == agent.PhaseInTurn })
		return inTurn
	})
}

// TestDeliveredTurnSignalsInWriteWindow 在放锁写 stdin 的窗口里注入 result(q=0)
// 与 idle。回合必须按 Accepted、TurnStarted、TurnEnded 的顺序结束，且 TurnEnded
// 的 turn_id 与 TurnStarted 相同。
func TestDeliveredTurnSignalsInWriteWindow(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "hold-stdin")
	driver, rec := newTestDriver(t, cfg)

	big := strings.Repeat("x", 256<<10)
	done := make(chan error, 1)
	go func() {
		done <- driver.Deliver(
			agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{big}},
			apiKeyCredential(),
		)
	}()

	waitInTurn(t, driver)
	// 写 stdin 尚未结束，边界信号不得抢先结束一个还没有 TurnStarted 的回合。
	feed(t, driver, resultLine("ses-hold", "done", false, 0), idleLine("ses-hold"))

	if err := os.WriteFile(filepath.Join(cfg.Home, ".fake", "release"), []byte("go"), 0o600); err != nil {
		t.Fatalf("release fake: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Deliver did not return after the write was released")
	}

	events := rec.wait(t, "delivered turn", func(events []sinkEvent) bool { return len(events) >= 3 })
	assertTurnEventInvariant(t, events)
	assertKinds(t, events[:3], eventAccepted, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "d-1" {
		t.Fatalf("accepted = %+v", events[0])
	}
	if events[1].DeliveryID != "d-1" || events[1].TurnID == "" {
		t.Fatalf("turn_started = %+v", events[1])
	}
	if events[2].TurnID != events[1].TurnID || events[2].AgentExited || events[2].Output != "done" {
		t.Fatalf("turn_ended = %+v", events[2])
	}
}

// TestProcessExitInWriteWindow 在放锁写 stdin 的窗口里注入进程退出。写入随 stdin
// 关闭而失败，这批投递没有被接受，也不能以它的 delivery_id 开始回合：回合外退出
// 补一对 delivery_id 为空的 TurnStarted 与 TurnEnded。
func TestProcessExitInWriteWindow(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "exit-hold-stdin")
	driver, rec := newTestDriver(t, cfg)

	big := strings.Repeat("x", 256<<10)
	done := make(chan error, 1)
	go func() {
		done <- driver.Deliver(
			agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{big}},
			apiKeyCredential(),
		)
	}()

	waitInTurn(t, driver)

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Deliver to a process that exited during the write succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Deliver did not return after the process exited during the write")
	}

	events := rec.wait(t, "exit turn_ended", func(events []sinkEvent) bool {
		for _, event := range events {
			if event.Kind == eventTurnEnded && event.AgentExited {
				return true
			}
		}
		return false
	})
	assertTurnEventInvariant(t, events)
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "" || events[0].TurnID == "" {
		t.Fatalf("turn_started = %+v, want an empty delivery id", events[0])
	}
	if events[1].TurnID != events[0].TurnID || !events[1].AgentExited {
		t.Fatalf("turn_ended = %+v", events[1])
	}
}

// TestBufferedExitAfterBoundaryEndsTurn 在放锁写 stdin 的窗口里注入 result(q=0)
// 与 idle，再让进程退出，随后写入成功。回合边界先到齐，必须按边界正常结束
// （agent_exited=false），缓冲的退出只能作为回合外退出单独报告，不能把已经完成
// 的回合改报成 agent_exited。
func TestBufferedExitAfterBoundaryEndsTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})
	driver.mu.Lock()
	driver.stdin = writeEnd
	driver.mu.Unlock()
	feed(t, driver, initLine("ses-buf"))

	big := strings.Repeat("x", 256<<10)
	done := make(chan error, 1)
	go func() {
		done <- driver.Deliver(
			agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{big}},
			agent.Credential{},
		)
	}()

	waitInTurn(t, driver)
	// 写入尚未结束：边界信号到齐，进程退出也被观察到。两者都留到写入
	// 结束、TurnStarted 发出之后判定。
	feed(t, driver, resultLine("ses-buf", "done", false, 0), idleLine("ses-buf"))
	driver.mu.Lock()
	driver.handleProcessExitLocked("exit status 1")
	driver.mu.Unlock()

	// 放行读端，让写入成功结束。
	go func() { _, _ = io.Copy(io.Discard, readEnd) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Deliver did not return after the write was drained")
	}

	events := rec.snapshot()
	assertTurnEventInvariant(t, events)
	assertKinds(t, events,
		eventAccepted, eventTurnStarted, eventTurnEnded, eventTurnStarted, eventTurnEnded)
	if events[1].DeliveryID != "d-1" || events[1].TurnID == "" {
		t.Fatalf("turn_started = %+v", events[1])
	}
	if events[2].TurnID != events[1].TurnID || events[2].AgentExited || events[2].Output != "done" {
		t.Fatalf("boundary turn_ended = %+v, want a normal end with output done", events[2])
	}
	if events[3].DeliveryID != "" || events[3].TurnID == "" || events[3].TurnID == events[1].TurnID {
		t.Fatalf("exit turn_started = %+v, want an outside-turn start", events[3])
	}
	if events[4].TurnID != events[3].TurnID || !events[4].AgentExited {
		t.Fatalf("exit turn_ended = %+v", events[4])
	}
}

// TestWriteSuccessAfterExitKeepsLastAcceptedSeq 覆盖写入成功但进程已经退出、
// 不会回 Accepted 的情况：last_accepted_seq 不得推进。
func TestWriteSuccessAfterExitKeepsLastAcceptedSeq(t *testing.T) {
	driver, rec := newBareDriver(t)
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	driver.mu.Lock()
	driver.stdin = writeEnd
	driver.mu.Unlock()
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})

	big := strings.Repeat("x", 256<<10)
	done := make(chan error, 1)
	go func() {
		done <- driver.Deliver(
			agent.Batch{DeliveryID: "d-1", ThroughSeq: 7, Texts: []string{big}},
			agent.Credential{},
		)
	}()

	waitInTurn(t, driver)
	// 模拟进程已经退出、退出事件也已处理；此时写入仍在进行，随后会成功。
	driver.mu.Lock()
	driver.phase = agent.PhaseExited
	driver.mu.Unlock()

	go func() {
		_, _ = io.Copy(io.Discard, readEnd)
	}()

	select {
	case err := <-done:
		if err != agent.ErrExited {
			t.Fatalf("Deliver = %v, want ErrExited", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Deliver did not return after the write was drained")
	}
	_ = writeEnd.Close()

	if events := rec.snapshot(); len(events) != 0 {
		t.Fatalf("Sink called though no Accepted could be reported: %+v", events)
	}
	var state agent.State
	driver.Snapshot(func(s agent.State) { state = s })
	if state.LastAcceptedSeq != 0 {
		t.Fatalf("LastAcceptedSeq = %d, want 0", state.LastAcceptedSeq)
	}
}

// TestStdoutEOFWaitIsBounded 覆盖进程退出后 stdout 仍被后台子进程持有：等待
// EOF 最多 2 s，随后关闭读端并照常报告退出。
func TestStdoutEOFWaitIsBounded(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "inherit-stdout")
	driver, rec := newTestDriver(t, cfg)

	begin := time.Now()
	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	events := rec.wait(t, "exit after the stdout EOF wait", func(events []sinkEvent) bool {
		for _, event := range events {
			if event.Kind == eventTurnEnded && event.AgentExited {
				return true
			}
		}
		return false
	})
	elapsed := time.Since(begin)
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("exit reported after %v, want about 2 s", elapsed)
	}
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("exit reported after %v, want about 2 s", elapsed)
	}
	assertTurnEventInvariant(t, events)
	if events[len(events)-1].Kind != eventTurnEnded || !events[len(events)-1].AgentExited {
		t.Fatalf("last event = %+v, want an agent exit turn_ended", events[len(events)-1])
	}
}
