package main

import (
	"net/http"
	"testing"
)

func TestRunGetRunning(t *testing.T) {
	srv, got := serveJSON(t, http.StatusOK, `{"run_id":"run-1","state":"running","substate":"thinking","failure_reason":"","turns":[{"turn_id":"turn-a","started_at":"2026-11-19T06:30:00.123Z","ended_at":"2026-11-19T06:30:05.000Z","output":"hello\nworld\n","is_error":false}],"suspensions":[]}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "get", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "run_id: run-1\n" +
		"state: running\n" +
		"substate: thinking\n" +
		"turns: 1\n" +
		"turn 1  turn-a  started 2026-11-19T06:30:00.123Z  ended 2026-11-19T06:30:05.000Z  is_error false\n" +
		"  hello\n" +
		"  world\n" +
		"suspensions: 0\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
	if got.method != http.MethodGet || got.path != "/v1/runs/run-1" {
		t.Errorf("request = %s %s, want GET /v1/runs/run-1", got.method, got.path)
	}
}

func TestRunGetSuspended(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, `{"run_id":"run-2","state":"suspended","substate":"","failure_reason":"","turns":[],"suspensions":[{"started_at":"2026-11-19T07:00:00.000Z","snapshot_ms":12,"restore_ms":null,"hello_ms":null,"error":"boom"}]}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "get", "run-2")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "run_id: run-2\n" +
		"state: suspended\n" +
		"substate: -\n" +
		"turns: 0\n" +
		"suspensions: 1\n" +
		"suspension 1  started 2026-11-19T07:00:00.000Z  snapshot_ms 12  restore_ms -  hello_ms -\n" +
		"  error: boom\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

func TestRunGetFailed(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, `{"run_id":"run-3","state":"failed","substate":"","failure_reason":"sandbox crashed","turns":[{"turn_id":"turn-a","started_at":"2026-11-19T08:00:00.000Z","ended_at":null,"output":"","is_error":true}],"suspensions":[]}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "get", "run-3")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "run_id: run-3\n" +
		"state: failed\n" +
		"substate: -\n" +
		"failure_reason: sandbox crashed\n" +
		"turns: 1\n" +
		"turn 1  turn-a  started 2026-11-19T08:00:00.000Z  ended -  is_error true\n" +
		"suspensions: 0\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

func TestRunGetBlankLineInOutput(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, `{"run_id":"run-4","state":"ended","substate":"done","failure_reason":"","turns":[{"turn_id":"turn-a","started_at":"2026-11-19T09:00:00.000Z","ended_at":"2026-11-19T09:00:01.000Z","output":"line one\n\nline two\n","is_error":false}],"suspensions":[]}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "get", "run-4")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "run_id: run-4\n" +
		"state: ended\n" +
		"substate: done\n" +
		"turns: 1\n" +
		"turn 1  turn-a  started 2026-11-19T09:00:00.000Z  ended 2026-11-19T09:00:01.000Z  is_error false\n" +
		"  line one\n" +
		"\n" +
		"  line two\n" +
		"suspensions: 0\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

func TestRunGetNotFound(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusNotFound, `{"error":"run not found"}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "get", "missing")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != "castworks: server returned 404: run not found\n" {
		t.Errorf("stderr = %q, want %q", stderr, "castworks: server returned 404: run not found\n")
	}
}
