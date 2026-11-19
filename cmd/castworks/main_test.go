package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/humanchannel/api"
)

// cliTimeout bounds every command line invocation in a test. A command that
// polls forever or retries a permanent error hits the deadline instead of
// hanging the test binary.
const cliTimeout = 5 * time.Second

// apiRequest is one request the fake server received.
type apiRequest struct {
	method string
	path   string
	body   []byte
}

// serveJSON starts a fake API server that records the request it receives and
// answers with status and body.
func serveJSON(t *testing.T, status int, body string) (*httptest.Server, *apiRequest) {
	t.Helper()
	got := &apiRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*got = apiRequest{method: r.Method, path: r.URL.Path, body: data}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// runCLI runs the command line with injected streams and returns the exit
// code and both streams. It fails the test when the command is still running
// at the deadline instead of leaving the test binary hung.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	var out, errOut strings.Builder
	code = run(ctx, args, &out, &errOut)
	if ctx.Err() != nil {
		t.Fatalf("castworks %s did not finish within %s", strings.Join(args, " "), cliTimeout)
	}
	return code, out.String(), errOut.String()
}

func TestRunCreate(t *testing.T) {
	srv, got := serveJSON(t, http.StatusCreated, `{"run_id":"run-1"}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "create", "hello world")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "run-1\n" {
		t.Errorf("stdout = %q, want %q", stdout, "run-1\n")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if got.method != http.MethodPost || got.path != "/v1/runs" {
		t.Errorf("request = %s %s, want POST /v1/runs", got.method, got.path)
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", got.body, err)
	}
	if body.Prompt != "hello world" {
		t.Errorf("prompt = %q, want %q", body.Prompt, "hello world")
	}
}

func TestRunSend(t *testing.T) {
	srv, got := serveJSON(t, http.StatusOK, `{"inbox_seq":42}`)

	code, stdout, stderr := runCLI(t, "run", "send", "--server", srv.URL, "run-1", "hello")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "42\n" {
		t.Errorf("stdout = %q, want %q", stdout, "42\n")
	}
	if got.method != http.MethodPost || got.path != "/v1/runs/run-1/messages" {
		t.Errorf("request = %s %s, want POST /v1/runs/run-1/messages", got.method, got.path)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", got.body, err)
	}
	if body.Text != "hello" {
		t.Errorf("text = %q, want %q", body.Text, "hello")
	}
}

func TestRunEnd(t *testing.T) {
	srv, got := serveJSON(t, http.StatusOK, `{}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "end", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if got.method != http.MethodPost || got.path != "/v1/runs/run-1/end" {
		t.Errorf("request = %s %s, want POST /v1/runs/run-1/end", got.method, got.path)
	}
}

func TestRunList(t *testing.T) {
	srv, got := serveJSON(t, http.StatusOK, `[{"run_id":"run-2","state":"running","substate":"","created_at":"2026-11-19T06:30:00.123Z"},{"run_id":"run-1","state":"ended","substate":"done","created_at":"2026-11-18T01:02:03.004Z"}]`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "list")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "RUN_ID  STATE    SUBSTATE  CREATED_AT\n" +
		"run-2   running  -         2026-11-19T06:30:00.123Z\n" +
		"run-1   ended    done      2026-11-18T01:02:03.004Z\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
	if got.method != http.MethodGet || got.path != "/v1/runs" {
		t.Errorf("request = %s %s, want GET /v1/runs", got.method, got.path)
	}
}

func TestRunListEmpty(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, `[]`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "list")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "RUN_ID  STATE  SUBSTATE  CREATED_AT\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestServerFlagAfterSubcommand(t *testing.T) {
	srv, got := serveJSON(t, http.StatusOK, `[]`)

	code, _, stderr := runCLI(t, "run", "list", "--server", srv.URL)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got.method != http.MethodGet {
		t.Errorf("request = %q, want the server after the subcommand to be used", got.method)
	}
}

func TestServerFlagLaterWins(t *testing.T) {
	first, firstReq := serveJSON(t, http.StatusOK, `[]`)
	second, _ := serveJSON(t, http.StatusOK, `[{"run_id":"run-2","state":"running","substate":"","created_at":"2026-11-19T06:30:00.123Z"}]`)

	code, stdout, stderr := runCLI(t, "--server", first.URL, "run", "list", "--server", second.URL)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if firstReq.method != "" {
		t.Errorf("the first --server was used; want the one after the subcommand")
	}
	if !strings.Contains(stdout, "run-2") {
		t.Errorf("stdout = %q, want the second server's run", stdout)
	}
}

func TestParseArgsDefaultServer(t *testing.T) {
	inv, err := parseArgs([]string{"run", "list"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if inv.server != "http://127.0.0.1:8080" {
		t.Errorf("server = %q, want %q", inv.server, "http://127.0.0.1:8080")
	}
	if inv.command != "list" || len(inv.args) != 0 {
		t.Errorf("command = %q args = %q, want list with no arguments", inv.command, inv.args)
	}
}

func TestParseArgsEqualityForm(t *testing.T) {
	inv, err := parseArgs([]string{"run", "list", "--server=http://example.com:9000"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if inv.server != "http://example.com:9000" {
		t.Errorf("server = %q, want %q", inv.server, "http://example.com:9000")
	}
}

func TestDoubleDashMakesPositional(t *testing.T) {
	srv, got := serveJSON(t, http.StatusCreated, `{"run_id":"run-1"}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "create", "--", "--weird")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "run-1\n" {
		t.Errorf("stdout = %q, want %q", stdout, "run-1\n")
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", got.body, err)
	}
	if body.Prompt != "--weird" {
		t.Errorf("prompt = %q, want %q", body.Prompt, "--weird")
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"run"},
		{"run", "bogus"},
		{"run", "create"},
		{"run", "create", "a", "b"},
		{"run", "send", "run-1"},
		{"run", "send", "run-1", "text", "extra"},
		{"run", "end"},
		{"run", "list", "extra"},
		{"run", "get"},
		{"run", "get", "run-1", "extra"},
		{"run", "watch"},
		{"run", "watch", "run-1", "extra"},
		{"--server"},
		{"run", "list", "--server"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "usage: castworks") {
				t.Errorf("stderr = %q, want the usage text", stderr)
			}
		})
	}
}

func TestInvalidServer(t *testing.T) {
	cases := []string{"", "ftp://example.com", "not a url", "http://"}
	for _, server := range cases {
		t.Run(server, func(t *testing.T) {
			code, _, stderr := runCLI(t, "--server", server, "run", "list")
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, "invalid server address") {
				t.Errorf("stderr = %q, want an invalid server address error", stderr)
			}
			if !strings.Contains(stderr, "usage: castworks") {
				t.Errorf("stderr = %q, want the usage text", stderr)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"run", "-h"}, {"run", "create", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stdout, "usage: castworks") {
				t.Errorf("stdout = %q, want the usage text", stdout)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestServerError(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusNotFound, `{"error":"run not found"}`)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "end", "missing")
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

func TestServerInternalError(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusInternalServerError, `{"error":"internal error"}`)

	code, _, stderr := runCLI(t, "--server", srv.URL, "run", "list")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr != "castworks: server returned 500: internal error\n" {
		t.Errorf("stderr = %q, want %q", stderr, "castworks: server returned 500: internal error\n")
	}
}

// TestFormatTimeConvertsToUTC pins the rendering: timestamps are converted to
// UTC and formatted with fixed millisecond precision, as the CLI contract
// requires.
func TestFormatTimeConvertsToUTC(t *testing.T) {
	cases := []struct {
		name string
		time time.Time
		want string
	}{
		{"utc", time.Date(2026, 11, 19, 6, 30, 0, 123456789, time.UTC), "2026-11-19T06:30:00.123Z"},
		{"positive offset", time.Date(2026, 11, 19, 14, 30, 0, 123000000, time.FixedZone("CST", 8*60*60)), "2026-11-19T06:30:00.123Z"},
		{"negative offset", time.Date(2026, 11, 18, 22, 30, 0, 123000000, time.FixedZone("", -8*60*60)), "2026-11-19T06:30:00.123Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTime(api.Time{Time: tc.time}); got != tc.want {
				t.Errorf("formatTime = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConnectionErrorNamesServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	code, _, stderr := runCLI(t, "--server", "http://"+addr, "run", "list")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, addr) {
		t.Errorf("stderr = %q, want it to contain the server address %q", stderr, addr)
	}
}

func TestPlaceholderCommands(t *testing.T) {
	for _, name := range []string{"get", "watch"} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, "run", name, "run-1")
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "not implemented") {
				t.Errorf("stderr = %q, want a not implemented message", stderr)
			}
		})
	}
}
