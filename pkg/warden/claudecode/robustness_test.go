package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// runnerFunc 让测试替换 ProcessRunner 并取得启动的 cmd。
type runnerFunc func(cmd *exec.Cmd) (<-chan error, error)

func (f runnerFunc) Start(cmd *exec.Cmd) (<-chan error, error) { return f(cmd) }

func apiKeyCredential() agent.Credential {
	return agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"}
}

// TestDeliverDoesNotHoldStateLockWhileWritingStdin 复现持状态锁写 stdin 的死锁：
// 假进程先连续写 256 KiB stdout、写完之前不读 stdin；回合中投递 256 KiB 的一批
// 必须在这个前提下仍能返回，Shutdown 也必须按期完成。
func TestDeliverDoesNotHoldStateLockWhileWritingStdin(t *testing.T) {
	cfg := testConfig(t)
	// 关停等待取小于测试上下文的值，竞态检测下假进程处理 256 KiB 输入更慢时
	// 也能按升级顺序退出。
	cfg.StdinCloseWait = 300 * time.Millisecond
	cfg.TerminateWait = 300 * time.Millisecond
	writeFakeMode(t, cfg.Home, "flood")

	rec := &recorder{}
	var cmdMu sync.Mutex
	var started *exec.Cmd
	runner := runnerFunc(func(cmd *exec.Cmd) (<-chan error, error) {
		cmdMu.Lock()
		started = cmd
		cmdMu.Unlock()
		return DefaultProcessRunner{}.Start(cmd)
	})
	driver := New(cfg, rec, runner)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = driver.Shutdown(ctx)
	}()

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver d-1: %v", err)
	}
	rec.wait(t, "turn started", func(events []sinkEvent) bool { return len(events) >= 2 })

	// 放行假进程的 256 KiB stdout 写入，同时开始回合中的大批投递。
	if err := os.WriteFile(filepath.Join(cfg.Home, ".fake", "flood-start"), []byte("go"), 0o600); err != nil {
		t.Fatalf("release flood: %v", err)
	}
	big := strings.Repeat("x", 256<<10)
	done := make(chan error, 1)
	go func() {
		done <- driver.Deliver(
			agent.Batch{DeliveryID: "d-2", ThroughSeq: 2, Texts: []string{big}},
			agent.Credential{},
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Deliver d-2: %v", err)
		}
	case <-time.After(5 * time.Second):
		cmdMu.Lock()
		cmd := started
		cmdMu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-done:
			_ = err
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("Deliver d-2 did not return within 5 s: stdin write is serialized by the state lock")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	begin := time.Now()
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("Shutdown took %v with a 1 s context", elapsed)
	}
}

// TestBoundaryQueuedResultInvalidatesIdle 覆盖回合中 q>0 的 result 使已到的 idle 作废。
func TestBoundaryQueuedResultInvalidatesIdle(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	feed(t, driver, idleLine("ses-1"))
	feed(t, driver, resultLine("ses-1", "first", false, 1))
	feed(t, driver, resultLine("ses-1", "second", false, 0))
	assertKinds(t, rec.snapshot(), eventTurnStarted)

	feed(t, driver, idleLine("ses-1"))
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[1].Output != "second" {
		t.Fatalf("turn_ended output = %q, want second", events[1].Output)
	}
}

// TestBoundaryRunningInvalidatesSignals 覆盖回合中会话状态变为 running 使 idle 与
// result 都作废。
func TestBoundaryRunningInvalidatesSignals(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	feed(t, driver, idleLine("ses-1"))
	feed(t, driver, runningLine("ses-1"))
	feed(t, driver, resultLine("ses-1", "done", false, 0))
	assertKinds(t, rec.snapshot(), eventTurnStarted)

	feed(t, driver, idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventTurnStarted, eventTurnEnded)
}

// TestDeliverDuringTurnInvalidatesSignals 覆盖回合中投递写入成功使 idle 与 result
// 都作废。
func TestDeliverDuringTurnInvalidatesSignals(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "silent")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver d-1: %v", err)
	}
	feed(t, driver, initLine("ses-1"))
	feed(t, driver, resultLine("ses-1", "first", false, 0))

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-2", ThroughSeq: 2, Texts: []string{"queued"}},
		agent.Credential{},
	); err != nil {
		t.Fatalf("Deliver d-2: %v", err)
	}
	feed(t, driver, idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventAccepted, eventTurnStarted, eventAccepted)

	feed(t, driver, resultLine("ses-1", "second", false, 0))
	assertKinds(t, rec.snapshot(), eventAccepted, eventTurnStarted, eventAccepted, eventTurnEnded)
}

// TestExitAfterBoundaryIsReportedAfterTurn 覆盖进程输出完整边界后立即退出时，
// 先报回合边界，再报回合外退出。
func TestExitAfterBoundaryIsReportedAfterTurn(t *testing.T) {
	for i := 0; i < 100; i++ {
		cfg := testConfig(t)
		writeFakeMode(t, cfg.Home, "exit-after-boundary")
		driver, rec := newTestDriver(t, cfg)

		if err := driver.Deliver(
			agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
			apiKeyCredential(),
		); err != nil {
			t.Fatalf("iteration %d: Deliver: %v", i, err)
		}
		events := rec.wait(t, "boundary then exit", func(events []sinkEvent) bool {
			ended := 0
			for _, event := range events {
				if event.Kind == eventTurnEnded {
					ended++
				}
			}
			return ended == 2
		})
		got := kinds(events)
		want := []string{eventAccepted, eventTurnStarted, eventTurnEnded, eventTurnStarted, eventTurnEnded}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: event kinds = %v, want %v (events: %+v)", i, got, want, events)
		}
		if events[2].AgentExited || events[2].Output != "done" {
			t.Fatalf("iteration %d: boundary turn_ended = %+v", i, events[2])
		}
		if !events[4].AgentExited {
			t.Fatalf("iteration %d: exit turn_ended = %+v", i, events[4])
		}
	}
}

// TestNoSinkAfterShutdownStarts 覆盖开始关停之后不再调用 Sink：假进程在 stdin
// 关闭后照常输出 result 与 idle，这些消息不得产生任何事件。
func TestNoSinkAfterShutdownStarts(t *testing.T) {
	cfg := testConfig(t)
	cfg.StdinCloseWait = 2 * time.Second
	cfg.TerminateWait = time.Second
	writeFakeMode(t, cfg.Home, "boundary-after-close")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	rec.wait(t, "turn started", func(events []sinkEvent) bool { return len(events) >= 2 })
	before := rec.count()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	streamPath := filepath.Join(cfg.Home, ".castworks", "stream.jsonl")
	waitFor(t, func() bool {
		data, err := os.ReadFile(streamPath)
		return err == nil && strings.Contains(string(data), `"state":"idle"`)
	})
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := rec.count(); got != before {
			t.Fatalf("Sink called after shutdown started: %+v", rec.snapshot()[before:])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConcurrentShutdownWaitsForProcessExit 覆盖并发的第二次 Shutdown 等到进程
// 退出后再返回。
func TestConcurrentShutdownWaitsForProcessExit(t *testing.T) {
	cfg := testConfig(t)
	cfg.StdinCloseWait = 300 * time.Millisecond
	cfg.TerminateWait = 300 * time.Millisecond
	writeFakeMode(t, cfg.Home, "shutdown")
	driver, _ := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	first := make(chan error, 1)
	go func() { first <- driver.Shutdown(ctx1) }()

	eventsPath := filepath.Join(cfg.Home, ".fake", "events.txt")
	waitFor(t, func() bool {
		data, err := os.ReadFile(eventsPath)
		return err == nil && strings.Contains(string(data), "stdin-close")
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	begin := time.Now()
	if err := driver.Shutdown(ctx2); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	elapsed := time.Since(begin)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("second Shutdown returned after %v, before the process exited", elapsed)
	}

	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("first Shutdown did not return")
	}
}

// TestStartCommandAndWorkDir 核对启动参数逐字与工作目录。
func TestStartCommandAndWorkDir(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "record")
	driver, _ := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	argsPath := filepath.Join(cfg.Home, ".fake", "args.json")
	var args []string
	waitFor(t, func() bool {
		data, err := os.ReadFile(argsPath)
		if err != nil {
			return false
		}
		return json.Unmarshal(data, &args) == nil
	})
	if !reflect.DeepEqual(args, claudeArgs) {
		t.Fatalf("start args = %q, want %q", args, claudeArgs)
	}

	cwdPath := filepath.Join(cfg.Home, ".fake", "cwd.txt")
	var cwd string
	waitFor(t, func() bool {
		data, err := os.ReadFile(cwdPath)
		if err != nil {
			return false
		}
		cwd = strings.TrimSpace(string(data))
		return cwd != ""
	})
	wantDir := filepath.Join(cfg.Home, "work")
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	if resolved, err := filepath.EvalSymlinks(wantDir); err == nil {
		wantDir = resolved
	}
	if cwd != wantDir {
		t.Fatalf("working directory = %q, want %q", cwd, wantDir)
	}
}

// TestShutdownDefaults 核对关停等待的默认值。
func TestShutdownDefaults(t *testing.T) {
	driver := New(Config{}, &recorder{}, nil)
	if driver.cfg.StdinCloseWait != 10*time.Second {
		t.Fatalf("StdinCloseWait default = %v, want 10 s", driver.cfg.StdinCloseWait)
	}
	if driver.cfg.TerminateWait != 5*time.Second {
		t.Fatalf("TerminateWait default = %v, want 5 s", driver.cfg.TerminateWait)
	}
}
