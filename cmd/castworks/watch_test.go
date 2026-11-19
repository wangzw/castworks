package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/humanchannel/client"
)

// pollStep is one response the fake polling server sends.
type pollStep struct {
	status int
	body   string
}

// servePolls starts a fake API server that answers the i-th request with the
// i-th step. The steps are a closed script: a request past the last step means
// the command polled when it should have stopped, so the server fails the test
// and answers with a permanent 400 to make the command exit. It returns the
// server and a counter of the requests it received.
func servePolls(t *testing.T, steps ...pollStep) (*httptest.Server, *int32) {
	t.Helper()
	var mu sync.Mutex
	var calls int32
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		idx := i
		i++
		mu.Unlock()
		if idx >= len(steps) {
			t.Errorf("fake server received request %d, want at most %d", idx+1, len(steps))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"unexpected poll"}`)
			return
		}
		step := steps[idx]
		status := step.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, step.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// setWatchInterval lowers the poll interval for the duration of the test.
func setWatchInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := watchPollInterval
	watchPollInterval = d
	t.Cleanup(func() { watchPollInterval = old })
}

// watchTestEnv builds an environment whose client talks to baseURL through
// hc.
func watchTestEnv(t *testing.T, baseURL string, hc *http.Client) (*environment, *strings.Builder, *strings.Builder) {
	t.Helper()
	c, err := client.New(baseURL, hc)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	var stdout, stderr strings.Builder
	return &environment{stdout: &stdout, stderr: &stderr, server: baseURL, client: c}, &stdout, &stderr
}

func TestRunWatchNewTurnsAndStateChanges(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	srv, _ := servePolls(t,
		pollStep{body: `{"run_id":"run-1","state":"running","substate":"thinking","failure_reason":"","turns":[{"turn_id":"t-b","started_at":"2026-11-19T06:00:02.000Z","ended_at":"2026-11-19T06:00:03.000Z","output":"b\n","is_error":false},{"turn_id":"t-a","started_at":"2026-11-19T06:00:01.000Z","ended_at":"2026-11-19T06:00:02.000Z","output":"a\n","is_error":true},{"turn_id":"t-p","started_at":"2026-11-19T06:00:03.000Z","ended_at":null,"output":"","is_error":false}],"suspensions":[]}`},
		pollStep{body: `{"run_id":"run-1","state":"running","substate":"thinking","failure_reason":"","turns":[{"turn_id":"t-p","started_at":"2026-11-19T06:00:03.000Z","ended_at":"2026-11-19T06:00:04.000Z","output":"p one\n\np two\n","is_error":false}],"suspensions":[]}`},
		pollStep{body: `{"run_id":"run-1","state":"ended","substate":"","failure_reason":"","turns":[],"suspensions":[]}`},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "turn t-a is_error=true\n" +
		"  a\n" +
		"turn t-b is_error=false\n" +
		"  b\n" +
		"state=running substate=thinking\n" +
		"turn t-p is_error=false\n" +
		"  p one\n" +
		"\n" +
		"  p two\n" +
		"state=ended substate=-\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

func TestRunWatchFailed(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	srv, _ := servePolls(t,
		pollStep{body: `{"run_id":"run-1","state":"failed","substate":"","failure_reason":"sandbox crashed","turns":[],"suspensions":[]}`},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	want := "state=failed substate=-\n" +
		"failure_reason: sandbox crashed\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
	if stderr != "castworks: run failed\n" {
		t.Errorf("stderr = %q, want %q", stderr, "castworks: run failed\n")
	}
}

func TestRunWatchRetries500Once(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	running := `{"run_id":"run-1","state":"running","substate":"","failure_reason":"","turns":[{"turn_id":"t-1","started_at":"2026-11-19T06:00:00.000Z","ended_at":"2026-11-19T06:00:01.000Z","output":"out\n","is_error":false}],"suspensions":[]}`
	ended := `{"run_id":"run-1","state":"ended","substate":"","failure_reason":"","turns":[],"suspensions":[]}`
	srv, _ := servePolls(t,
		pollStep{status: http.StatusInternalServerError, body: `{"error":"internal error"}`},
		pollStep{status: http.StatusInternalServerError, body: `{"error":"internal error"}`},
		pollStep{body: running},
		pollStep{body: running},
		pollStep{body: ended},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "castworks: server returned 500: internal error, retrying\n" {
		t.Errorf("stderr = %q, want exactly one retry line", stderr)
	}
	want := "turn t-1 is_error=false\n" +
		"  out\n" +
		"state=running substate=-\n" +
		"state=ended substate=-\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

// failOnceTransport fails the first request with a transport error and then
// delegates to inner.
type failOnceTransport struct {
	fails int
	inner http.RoundTripper
}

func (f *failOnceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("connection refused")
	}
	return f.inner.RoundTrip(req)
}

func TestRunWatchRetriesConnectionError(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	running := `{"run_id":"run-1","state":"running","substate":"","failure_reason":"","turns":[{"turn_id":"t-1","started_at":"2026-11-19T06:00:00.000Z","ended_at":"2026-11-19T06:00:01.000Z","output":"out\n","is_error":false}],"suspensions":[]}`
	ended := `{"run_id":"run-1","state":"ended","substate":"","failure_reason":"","turns":[],"suspensions":[]}`
	srv, _ := servePolls(t,
		pollStep{body: running},
		pollStep{body: ended},
	)
	hc := &http.Client{Transport: &failOnceTransport{fails: 1, inner: http.DefaultTransport}}
	env, stdout, stderr := watchTestEnv(t, srv.URL, hc)

	code := runWatch(context.Background(), env, []string{"run-1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	wantStderr := "castworks: Get \"" + srv.URL + "/v1/runs/run-1\": connection refused, retrying\n"
	if stderr.String() != wantStderr {
		t.Errorf("stderr = %q, want one retry line %q", stderr.String(), wantStderr)
	}
	want := "turn t-1 is_error=false\n" +
		"  out\n" +
		"state=running substate=-\n" +
		"state=ended substate=-\n"
	if stdout.String() != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout.String(), want)
	}
}

func TestRunWatchNotFoundStops(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	srv, calls := servePolls(t,
		pollStep{status: http.StatusNotFound, body: `{"error":"run not found"}`},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "missing")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != "castworks: server returned 404: run not found\n" {
		t.Errorf("stderr = %q, want %q", stderr, "castworks: server returned 404: run not found\n")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server received %d requests, want 1 (a 404 must not be retried)", got)
	}
}

func TestRunWatchSubstateChange(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	delivering := `{"run_id":"run-1","state":"active","substate":"delivering","failure_reason":"","turns":[],"suspensions":[]}`
	inTurn := `{"run_id":"run-1","state":"active","substate":"in_turn","failure_reason":"","turns":[],"suspensions":[]}`
	ended := `{"run_id":"run-1","state":"ended","substate":"","failure_reason":"","turns":[],"suspensions":[]}`
	srv, calls := servePolls(t,
		pollStep{body: delivering},
		pollStep{body: delivering},
		pollStep{body: inTurn},
		pollStep{body: ended},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "state=active substate=delivering\n" +
		"state=active substate=in_turn\n" +
		"state=ended substate=-\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Errorf("server received %d requests, want 4", got)
	}
}

func TestRunWatchRetriesAgainAfterRecovery(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	running := `{"run_id":"run-1","state":"running","substate":"","failure_reason":"","turns":[],"suspensions":[]}`
	ended := `{"run_id":"run-1","state":"ended","substate":"","failure_reason":"","turns":[],"suspensions":[]}`
	srv, _ := servePolls(t,
		pollStep{status: http.StatusInternalServerError, body: `{"error":"internal error"}`},
		pollStep{body: running},
		pollStep{status: http.StatusBadGateway, body: `{"error":"bad gateway"}`},
		pollStep{body: ended},
	)

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	wantStderr := "castworks: server returned 500: internal error, retrying\n" +
		"castworks: server returned 502: bad gateway, retrying\n"
	if stderr != wantStderr {
		t.Errorf("stderr = %q, want one retry line per failure run %q", stderr, wantStderr)
	}
	want := "state=running substate=-\n" +
		"state=ended substate=-\n"
	if stdout != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", stdout, want)
	}
}

func TestRunWatchDecodeErrorStops(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	srv, calls := servePolls(t, pollStep{body: "not json"})

	code, stdout, stderr := runCLI(t, "--server", srv.URL, "run", "watch", "run-1")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server received %d requests, want 1 (a decode error must not be retried)", got)
	}
	if !strings.HasPrefix(stderr, "castworks: decode response: ") {
		t.Errorf("stderr = %q, want a decode response error", stderr)
	}
	if strings.Contains(stderr, "retrying") {
		t.Errorf("stderr = %q, want no retry line for a decode error", stderr)
	}
}

func TestRunWatchCancelledBeforeFirstPoll(t *testing.T) {
	setWatchInterval(t, time.Millisecond)
	srv, _ := servePolls(t,
		pollStep{body: `{"run_id":"run-1","state":"running","substate":"","failure_reason":"","turns":[],"suspensions":[]}`},
	)
	env, stdout, stderr := watchTestEnv(t, srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := runWatch(ctx, env, []string{"run-1"}); code != 130 {
		t.Fatalf("exit code = %d, want 130", code)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunWatchCancelledWhileWaiting(t *testing.T) {
	setWatchInterval(t, time.Hour)
	srv, _ := servePolls(t,
		pollStep{body: `{"run_id":"run-1","state":"running","substate":"","failure_reason":"","turns":[],"suspensions":[]}`},
	)
	env, _, stderr := watchTestEnv(t, srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)

	done := make(chan int, 1)
	go func() { done <- runWatch(ctx, env, []string{"run-1"}) }()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("exit code = %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWatch did not stop after the context was cancelled")
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
