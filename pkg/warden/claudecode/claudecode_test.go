package claudecode

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

const (
	eventAccepted    = "Accepted"
	eventTurnStarted = "TurnStarted"
	eventTurnEnded   = "TurnEnded"
)

type sinkEvent struct {
	Kind        string
	DeliveryID  string
	ThroughSeq  int64
	TurnID      string
	SessionID   string
	Output      string
	IsError     bool
	AgentExited bool
}

type recorder struct {
	mu     sync.Mutex
	events []sinkEvent
}

func (r *recorder) Accepted(deliveryID string, throughSeq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{
		Kind: eventAccepted, DeliveryID: deliveryID, ThroughSeq: throughSeq,
	})
}

func (r *recorder) TurnStarted(turnID, deliveryID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{
		Kind: eventTurnStarted, TurnID: turnID, DeliveryID: deliveryID, SessionID: sessionID,
	})
}

func (r *recorder) TurnEnded(turnID, output string, isError, agentExited bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{
		Kind: eventTurnEnded, TurnID: turnID, Output: output,
		IsError: isError, AgentExited: agentExited,
	})
}

func (r *recorder) snapshot() []sinkEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sinkEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recorder) wait(t *testing.T, what string, pred func([]sinkEvent) bool) []sinkEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events := r.snapshot()
		if pred(events) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; events: %+v", what, events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(t *testing.T) Config {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Config{
		ClaudeBin:      executable,
		Home:           t.TempDir(),
		LocalPATH:      "/test/local/bin",
		StdinCloseWait: 100 * time.Millisecond,
		TerminateWait:  100 * time.Millisecond,
		Logger:         discardLogger(),
	}
}

func newTestDriver(t *testing.T, cfg Config) (*Driver, *recorder) {
	t.Helper()
	rec := &recorder{}
	driver := New(cfg, rec, DefaultProcessRunner{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = driver.Shutdown(ctx)
	})
	return driver, rec
}

func writeFakeMode(t *testing.T, home, mode string) {
	t.Helper()
	base := filepath.Join(home, ".fake")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir .fake: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "mode"), []byte(mode+"\n"), 0o600); err != nil {
		t.Fatalf("write mode: %v", err)
	}
}

func releaseFake(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, ".fake", "release"), []byte("go"), 0o600); err != nil {
		t.Fatalf("write release: %v", err)
	}
}

// feed 把一行 stdout 交给与 readStdout 相同的处理路径。
func feed(t *testing.T, driver *Driver, lines ...string) {
	t.Helper()
	for _, line := range lines {
		driver.handleStdoutLine([]byte(line + "\n"))
	}
}

// newBareDriver 返回一个没有子进程、处于回合边界的驱动器，供逐行输入测试。
func newBareDriver(t *testing.T) (*Driver, *recorder) {
	t.Helper()
	rec := &recorder{}
	driver := New(Config{Home: t.TempDir(), Logger: discardLogger()}, rec, nil)
	driver.phase = agent.PhaseBoundary
	return driver, rec
}

// simulateDeliver 走 Deliver 在边界上的状态转换，但不写 stdin。
func simulateDeliver(driver *Driver, deliveryID string) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.beginTurnLocked()
	driver.announceTurnStartLocked(deliveryID)
}

func jsonLine(value map[string]any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func initLine(sessionID string) string {
	return jsonLine(map[string]any{
		"type": "system", "subtype": "init", "session_id": sessionID, "uuid": "u-init",
	})
}

func assistantLine(sessionID, text string) string {
	return jsonLine(map[string]any{
		"type": "assistant", "session_id": sessionID,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": text},
		}},
		"uuid": "u-assistant",
	})
}

func resultLine(sessionID, output string, isError bool, queued int) string {
	return jsonLine(map[string]any{
		"type": "result", "subtype": "success", "session_id": sessionID,
		"result": output, "is_error": isError, "queued_turn_count": queued,
		"uuid": "u-result",
	})
}

func idleLine(sessionID string) string {
	return jsonLine(map[string]any{
		"type": "system", "subtype": "session_state_changed",
		"state": "idle", "session_id": sessionID, "uuid": "u-idle",
	})
}

func runningLine(sessionID string) string {
	return jsonLine(map[string]any{
		"type": "system", "subtype": "session_state_changed",
		"state": "running", "session_id": sessionID, "uuid": "u-running",
	})
}

func kinds(events []sinkEvent) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.Kind
	}
	return out
}

func assertKinds(t *testing.T, events []sinkEvent, want ...string) {
	t.Helper()
	got := kinds(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds = %v, want %v (events: %+v)", got, want, events)
	}
}

func TestBoundaryResultBeforeIdle(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	feed(t, driver, resultLine("ses-1", "4", false, 0))
	assertKinds(t, rec.snapshot(), eventTurnStarted)

	feed(t, driver, idleLine("ses-1"))
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "d-1" || events[0].SessionID != "ses-1" {
		t.Fatalf("turn_started = %+v", events[0])
	}
	if events[1].TurnID != events[0].TurnID || events[1].Output != "4" || events[1].IsError || events[1].AgentExited {
		t.Fatalf("turn_ended = %+v", events[1])
	}
}

func TestBoundaryIdleBeforeResult(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	feed(t, driver, idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventTurnStarted)

	feed(t, driver, resultLine("ses-1", "ok", false, 0))
	assertKinds(t, rec.snapshot(), eventTurnStarted, eventTurnEnded)
}

func TestBoundaryQueuedTurnCount(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	feed(t, driver, resultLine("ses-1", "first", false, 1), idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventTurnStarted)

	feed(t, driver, assistantLine("ses-1", "second"), resultLine("ses-1", "second", false, 0))
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[1].Output != "second" {
		t.Fatalf("turn_ended output = %q, want second", events[1].Output)
	}
}

func TestSpontaneousRunningStartsTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")
	feed(t, driver, resultLine("ses-1", "first", false, 0), idleLine("ses-1"))

	feed(t, driver, runningLine("ses-1"))
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded, eventTurnStarted)
	if events[2].DeliveryID != "" || events[2].SessionID != "ses-1" || events[2].TurnID == events[0].TurnID {
		t.Fatalf("spontaneous turn_started = %+v", events[2])
	}

	feed(t, driver, resultLine("ses-1", "spontaneous", false, 0), idleLine("ses-1"))
	events = rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded, eventTurnStarted, eventTurnEnded)
	if events[3].Output != "spontaneous" || events[3].TurnID != events[2].TurnID {
		t.Fatalf("spontaneous turn_ended = %+v", events[3])
	}
}

func TestSpontaneousAssistantStartsTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")
	feed(t, driver, resultLine("ses-1", "first", false, 0), idleLine("ses-1"))

	feed(t, driver, assistantLine("ses-1", "task finished"))
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded, eventTurnStarted)
	if events[2].DeliveryID != "" {
		t.Fatalf("spontaneous turn_started = %+v", events[2])
	}

	feed(t, driver, resultLine("ses-1", "task finished", false, 0), idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventTurnStarted, eventTurnEnded, eventTurnStarted, eventTurnEnded)
}

func TestBoundaryNoiseDoesNotStartTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")
	feed(t, driver, resultLine("ses-1", "first", false, 0), idleLine("ses-1"))

	before := rec.count()
	feed(t, driver,
		idleLine("ses-1"),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "session_id": "ses-1"}),
		"this is not json",
		jsonLine(map[string]any{"type": "task_progress", "session_id": "ses-1"}),
	)
	if got := rec.count(); got != before {
		t.Fatalf("noise produced %d events: %+v", got-before, rec.snapshot()[before:])
	}

	var state agent.State
	driver.Snapshot(func(s agent.State) { state = s })
	if state.Phase != agent.PhaseBoundary || state.TurnID != "" {
		t.Fatalf("state = %+v, want boundary", state)
	}
}

func TestDeliverDuringTurn(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "silent")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"a", "b"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver d-1: %v", err)
	}
	feed(t, driver, initLine("ses-1"))
	rec.wait(t, "first turn started", func(events []sinkEvent) bool {
		return len(events) >= 2 && events[1].Kind == eventTurnStarted
	})

	var state agent.State
	driver.Snapshot(func(s agent.State) { state = s })
	if state.Phase != agent.PhaseInTurn || len(state.TurnID) != 32 {
		t.Fatalf("state = %+v, want in_turn with a 32-hex turn id", state)
	}
	if _, err := hex.DecodeString(state.TurnID); err != nil {
		t.Fatalf("turn id %q is not hex: %v", state.TurnID, err)
	}

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-2", ThroughSeq: 2, Texts: []string{"queued"}},
		agent.Credential{},
	); err != nil {
		t.Fatalf("Deliver d-2: %v", err)
	}
	events := rec.snapshot()
	assertKinds(t, events, eventAccepted, eventTurnStarted, eventAccepted)
	if events[2].DeliveryID != "d-2" || events[2].ThroughSeq != 2 {
		t.Fatalf("accepted = %+v", events[2])
	}

	feed(t, driver, resultLine("ses-1", "first", false, 1), idleLine("ses-1"))
	assertKinds(t, rec.snapshot(), eventAccepted, eventTurnStarted, eventAccepted)

	feed(t, driver, resultLine("ses-1", "drained", false, 0))
	assertKinds(t, rec.snapshot(), eventAccepted, eventTurnStarted, eventAccepted, eventTurnEnded)
	end := rec.snapshot()[3]
	if end.TurnID != events[1].TurnID || end.Output != "drained" || end.AgentExited {
		t.Fatalf("turn_ended = %+v", end)
	}

	driver.Snapshot(func(s agent.State) {
		if s.Phase != agent.PhaseBoundary || s.LastAcceptedSeq != 2 || s.LastTurn == nil {
			t.Fatalf("state = %+v", s)
		}
	})

	// 假进程记录下的 stdin 行应是两批以空行连接的用户消息。
	waitFor(t, func() bool {
		data, err := os.ReadFile(filepath.Join(cfg.Home, ".fake", "stdin.txt"))
		return err == nil && strings.Count(string(data), "\n") >= 2
	})
	data, err := os.ReadFile(filepath.Join(cfg.Home, ".fake", "stdin.txt"))
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdin has %d lines: %q", len(lines), data)
	}
	if text, err := userMessageText([]byte(lines[0])); err != nil || text != "a\n\nb" {
		t.Fatalf("first user message = %q, %v", text, err)
	}
	if text, err := userMessageText([]byte(lines[1])); err != nil || text != "queued" {
		t.Fatalf("second user message = %q, %v", text, err)
	}
}

func TestEncodeUserMessage(t *testing.T) {
	line, err := encodeUserMessage([]string{"one", "two"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("encoded line has no trailing newline: %q", line)
	}
	if text, err := userMessageText(line); err != nil || text != "one\n\ntwo" {
		t.Fatalf("text = %q, %v", text, err)
	}
	if !strings.Contains(string(line), `"type":"user"`) {
		t.Fatalf("encoded message is not a user message: %s", line)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", 5*time.Second)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
