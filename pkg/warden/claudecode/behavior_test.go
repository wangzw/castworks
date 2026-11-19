package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

func TestEnvironment(t *testing.T) {
	imagePATH := dockerfilePATH(t)
	cases := []struct {
		name     string
		cred     agent.Credential
		actor    bool
		wantPATH string
	}{
		{
			name:     "api key in actor mode",
			cred:     agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
			actor:    true,
			wantPATH: imagePATH,
		},
		{
			name:     "oauth token in local mode",
			cred:     agent.Credential{Kind: agent.CredentialOAuthToken, Value: "oauth-test"},
			actor:    false,
			wantPATH: "/test/local/bin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.ActorMode = tc.actor
			writeFakeMode(t, cfg.Home, "env")
			t.Setenv("CASTWORKS_LEAK", "leak")

			driver, _ := newTestDriver(t, cfg)
			if err := driver.Deliver(
				agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
				tc.cred,
			); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			env := envMap(t, waitEnv(t, filepath.Join(cfg.Home, ".fake", "env.json")))
			want := map[string]string{
				"HOME":                                  cfg.Home,
				"PATH":                                  tc.wantPATH,
				"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				"DISABLE_AUTOUPDATER":                      "1",
			}
			switch tc.cred.Kind {
			case agent.CredentialAPIKey:
				want["ANTHROPIC_API_KEY"] = tc.cred.Value
			case agent.CredentialOAuthToken:
				want["CLAUDE_CODE_OAUTH_TOKEN"] = tc.cred.Value
			}
			if len(env) != len(want) {
				t.Fatalf("environment has %d entries, want %d: %v", len(env), len(want), env)
			}
			for key, value := range want {
				if got, ok := env[key]; !ok || got != value {
					t.Fatalf("environment[%s] = %q (present %v), want %q; all: %v", key, got, ok, value, env)
				}
			}
		})
	}
}

func TestLocalPATHDefaultsToStartupPATH(t *testing.T) {
	t.Setenv("PATH", "/warden/startup/bin")
	cfg := testConfig(t)
	cfg.LocalPATH = ""
	writeFakeMode(t, cfg.Home, "env")

	driver, _ := newTestDriver(t, cfg)
	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	env := envMap(t, waitEnv(t, filepath.Join(cfg.Home, ".fake", "env.json")))
	if env["PATH"] != "/warden/startup/bin" {
		t.Fatalf("PATH = %q, want the startup PATH", env["PATH"])
	}
}

func envMap(t *testing.T, entries []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("environment entry %q has no =", entry)
		}
		if _, duplicate := out[key]; duplicate {
			t.Fatalf("environment entry %q is duplicated", key)
		}
		out[key] = value
	}
	return out
}

func waitEnv(t *testing.T, path string) []string {
	t.Helper()
	var env []string
	waitFor(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var parsed []string
		if err := json.Unmarshal(data, &parsed); err != nil || len(parsed) == 0 {
			return false
		}
		env = parsed
		return true
	})
	return env
}

func dockerfilePATH(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "actor", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	match := regexp.MustCompile(`(?m)^ENV PATH=(.+)$`).FindSubmatch(data)
	if match == nil {
		t.Fatalf("Dockerfile has no ENV PATH line")
	}
	return strings.TrimSpace(string(match[1]))
}

func TestStreamJSONLMatchesStdout(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "script")
	script := strings.Join([]string{
		initLine("ses-stream"),
		"this line is not json",
		resultLine("ses-stream", "half", false, 0),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cfg.Home, ".fake", "stdout.jsonl"), []byte(script), 0o600); err != nil {
		t.Fatalf("write fake stdout: %v", err)
	}

	driver, _ := newTestDriver(t, cfg)
	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	streamPath := filepath.Join(cfg.Home, ".castworks", "stream.jsonl")
	var content []byte
	waitFor(t, func() bool {
		data, err := os.ReadFile(streamPath)
		if err != nil {
			return false
		}
		content = data
		return string(data) == script
	})
	if string(content) != script {
		t.Fatalf("stream.jsonl = %q, want %q", content, script)
	}
	info, err := os.Stat(streamPath)
	if err != nil {
		t.Fatalf("stat stream.jsonl: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("stream.jsonl mode = %o, want 600", perm)
	}
}

func TestStderrForwarded(t *testing.T) {
	cfg := testConfig(t)
	stderrPath := filepath.Join(cfg.Home, "stderr.txt")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr file: %v", err)
	}
	cfg.Stderr = stderr
	t.Cleanup(func() { _ = stderr.Close() })
	writeFakeMode(t, cfg.Home, "stderr")

	driver, _ := newTestDriver(t, cfg)
	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	waitFor(t, func() bool {
		data, err := os.ReadFile(stderrPath)
		return err == nil && strings.Contains(string(data), "fake claude stderr")
	})
}

func TestExitInTurn(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "exit-in-turn")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	releaseFake(t, cfg.Home)

	events := rec.wait(t, "exit turn_ended", func(events []sinkEvent) bool {
		for _, event := range events {
			if event.Kind == eventTurnEnded && event.AgentExited {
				return true
			}
		}
		return false
	})
	end := events[len(events)-1]
	if end.Kind != eventTurnEnded || !end.IsError || !end.AgentExited {
		t.Fatalf("turn_ended = %+v", end)
	}
	if end.Output != "claude code exited: exit status 1" {
		t.Fatalf("turn_ended output = %q", end.Output)
	}
	var started sinkEvent
	for _, event := range events {
		if event.Kind == eventTurnStarted && event.TurnID == end.TurnID {
			started = event
		}
	}
	if started.Kind == "" {
		t.Fatalf("no turn_started for %s: %+v", end.TurnID, events)
	}

	driver.Snapshot(func(state agent.State) {
		if state.Phase != agent.PhaseExited {
			t.Fatalf("phase = %v, want exited", state.Phase)
		}
	})
	if err := driver.Deliver(agent.Batch{DeliveryID: "d-2", ThroughSeq: 2, Texts: []string{"x"}},
		agent.Credential{}); err != agent.ErrExited {
		t.Fatalf("Deliver after exit = %v, want ErrExited", err)
	}
}

func TestExitAtBoundary(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "exit-at-boundary")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	events := rec.wait(t, "boundary turn_ended", func(events []sinkEvent) bool {
		for _, event := range events {
			if event.Kind == eventTurnEnded && !event.AgentExited {
				return true
			}
		}
		return false
	})
	if events[len(events)-1].Output != "done" {
		t.Fatalf("boundary turn_ended = %+v", events[len(events)-1])
	}

	releaseFake(t, cfg.Home)
	events = rec.wait(t, "exit after boundary", func(events []sinkEvent) bool {
		ended := 0
		for _, event := range events {
			if event.Kind == eventTurnEnded {
				ended++
			}
		}
		return ended == 2
	})
	assertKinds(t, events, eventAccepted, eventTurnStarted, eventTurnEnded, eventTurnStarted, eventTurnEnded)
	exitStart := events[3]
	exitEnd := events[4]
	if exitStart.DeliveryID != "" || exitStart.TurnID != exitEnd.TurnID {
		t.Fatalf("exit turn_started = %+v, turn_ended = %+v", exitStart, exitEnd)
	}
	if !exitEnd.IsError || !exitEnd.AgentExited || exitEnd.Output != "claude code exited: exit status 0" {
		t.Fatalf("exit turn_ended = %+v", exitEnd)
	}

	driver.Snapshot(func(state agent.State) {
		if state.Phase != agent.PhaseExited {
			t.Fatalf("phase = %v, want exited", state.Phase)
		}
	})
}

func TestStartupFailure(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeBin = filepath.Join(cfg.Home, "no-such-claude")
	driver, rec := newTestDriver(t, cfg)

	err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	)
	if err == nil {
		t.Fatalf("Deliver with a missing binary succeeded")
	}
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "" || events[0].TurnID != events[1].TurnID {
		t.Fatalf("startup failure events = %+v", events)
	}
	if !events[1].IsError || !events[1].AgentExited ||
		!strings.HasPrefix(events[1].Output, "claude code exited: ") {
		t.Fatalf("startup failure turn_ended = %+v", events[1])
	}
	driver.Snapshot(func(state agent.State) {
		if state.Phase != agent.PhaseExited {
			t.Fatalf("phase = %v, want exited", state.Phase)
		}
	})
}

func TestWriteFailureOutsideTurn(t *testing.T) {
	cfg := testConfig(t)
	driver, rec := newTestDriver(t, cfg)
	driver.mu.Lock()
	driver.phase = agent.PhaseBoundary
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = readEnd.Close()
	driver.stdin = writeEnd
	driver.mu.Unlock()
	t.Cleanup(func() { _ = writeEnd.Close() })

	err = driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{},
	)
	if err == nil {
		t.Fatalf("Deliver to a closed pipe succeeded")
	}
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "" {
		t.Fatalf("write failure turn_started = %+v", events[0])
	}
	if !events[1].IsError || !events[1].AgentExited {
		t.Fatalf("write failure turn_ended = %+v", events[1])
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "all of it"
	if got := truncateOutput(short); got != short {
		t.Fatalf("short output changed: %q", got)
	}

	long := strings.Repeat("a", maxOutputBytes+10)
	got := truncateOutput(long)
	if len(got) > maxOutputBytes {
		t.Fatalf("truncated length = %d, want <= %d", len(got), maxOutputBytes)
	}
	if !strings.HasSuffix(got, "\n[output truncated: "+strconv.Itoa(maxOutputBytes+10)+" bytes]") {
		t.Fatalf("truncated output has no suffix: %q", got[len(got)-60:])
	}

	multibyte := strings.Repeat("中", maxOutputBytes/3+100)
	got = truncateOutput(multibyte)
	if len(got) > maxOutputBytes {
		t.Fatalf("multibyte truncated length = %d, want <= %d", len(got), maxOutputBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("multibyte truncated output is not valid UTF-8")
	}
	if !strings.Contains(got, "[output truncated: "+strconv.Itoa(len(multibyte))+" bytes]") {
		t.Fatalf("multibyte output has no suffix")
	}
}

func TestTruncatedOutputReachesSinkAndLastTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	huge := strings.Repeat("中", maxOutputBytes/3+100)
	feed(t, driver, resultLine("ses-1", huge, false, 0), idleLine("ses-1"))

	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	end := events[1]
	if len(end.Output) > maxOutputBytes || !utf8.ValidString(end.Output) {
		t.Fatalf("turn_ended output length = %d, valid = %v", len(end.Output), utf8.ValidString(end.Output))
	}
	driver.Snapshot(func(state agent.State) {
		if state.LastTurn == nil || state.LastTurn.Output != end.Output || state.LastTurn.TurnID != end.TurnID {
			t.Fatalf("last turn = %+v, turn_ended = %+v", state.LastTurn, end)
		}
	})
}

func TestWriteFailureInTurn(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-1"))
	simulateDeliver(driver, "d-1")

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = readEnd.Close()
	driver.mu.Lock()
	driver.stdin = writeEnd
	driver.mu.Unlock()
	t.Cleanup(func() { _ = writeEnd.Close() })

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-2", ThroughSeq: 2, Texts: []string{"queued"}},
		agent.Credential{},
	); err == nil {
		t.Fatalf("Deliver to a closed pipe succeeded")
	}
	events := rec.snapshot()
	assertKinds(t, events, eventTurnStarted, eventTurnEnded)
	if events[0].DeliveryID != "d-1" {
		t.Fatalf("turn_started = %+v", events[0])
	}
	if events[1].TurnID != events[0].TurnID || !events[1].IsError || !events[1].AgentExited {
		t.Fatalf("turn_ended = %+v, want the in-progress turn to end with agent_exited", events[1])
	}
	driver.Snapshot(func(state agent.State) {
		if state.Phase != agent.PhaseExited {
			t.Fatalf("phase = %v, want exited", state.Phase)
		}
	})
}

func TestShutdownEscalates(t *testing.T) {
	cfg := testConfig(t)
	cfg.StdinCloseWait = 80 * time.Millisecond
	cfg.TerminateWait = 80 * time.Millisecond
	writeFakeMode(t, cfg.Home, "shutdown")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	rec.wait(t, "fake started", func(events []sinkEvent) bool {
		return len(events) >= 2
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 120*time.Millisecond {
		t.Fatalf("Shutdown returned after %v, want at least the two waits", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v", elapsed)
	}

	data, err := os.ReadFile(filepath.Join(cfg.Home, ".fake", "events.txt"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("fake recorded %d events, want stdin-close and sigterm: %q", len(lines), data)
	}
	firstName, firstAt := parseFakeEvent(t, lines[0])
	secondName, secondAt := parseFakeEvent(t, lines[1])
	if firstName != "stdin-close" || secondName != "sigterm" {
		t.Fatalf("fake events = %q, %q; want stdin-close then sigterm", firstName, secondName)
	}
	if gap := time.Duration(secondAt - firstAt); gap < 50*time.Millisecond {
		t.Fatalf("SIGTERM came %v after stdin close, want the configured wait", gap)
	}

	driver.Snapshot(func(state agent.State) {
		if state.Phase != agent.PhaseExited {
			t.Fatalf("phase = %v, want exited", state.Phase)
		}
	})
}

func parseFakeEvent(t *testing.T, line string) (string, int64) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("malformed fake event %q", line)
	}
	at, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("malformed timestamp in %q: %v", line, err)
	}
	return fields[0], at
}
