package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

// unreachableDatabaseURL points at a closed port with a distinctive password.
// Any attempt to connect fails immediately; the tests only reach this URL if
// startup got past config validation and credential reading.
const unreachableDatabaseURL = "postgres://castworks:unreachable-secret@127.0.0.1:1/castworks?sslmode=disable&connect_timeout=1"

// testLogs is the concurrency-safe buffer TestMain installs as the slog
// default logger, the same way main does. Tests assert on what the process
// logged.
var testLogs = &lockedBuffer{}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(testLogs, nil)))
	os.Exit(m.Run())
}

// lockedBuffer is a bytes.Buffer safe for the concurrent goroutines that log
// during a test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logsSince returns the text written after the first offset bytes. The buffer
// only ever appends, so an earlier offset stays valid.
func logsSince(offset int) string {
	text := testLogs.String()
	if offset > len(text) {
		return ""
	}
	return text[offset:]
}

// runTestCredentialFile writes a non-empty credential file and returns its
// path.
func runTestCredentialFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llm-credential")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	return path
}

// runTestArgs returns a complete, valid --provider local command line except
// for the flags a test overrides.
func runTestArgs(credentialFile string) []string {
	return []string{
		"--database-url", unreachableDatabaseURL,
		"--llm-credential-file", credentialFile,
		"--llm-credential-kind", "oauth_token",
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--shutdown-timeout", "1s",
	}
}

// TestRunConfigErrorStopsBeforeAnyIO asserts that an invalid command line is
// reported as a usage error, with no listener and no database connection
// attempted. The database URL points at a closed port; a connection error
// would prove the ordering wrong.
func TestRunConfigErrorStopsBeforeAnyIO(t *testing.T) {
	credentialFile := runTestCredentialFile(t, "run-config-token")
	args := runTestArgs(credentialFile)

	// Replace the valid HTTP address with a non-loopback one. Config parsing
	// must reject it before anything else runs.
	args = replaceFlag(t, args, "http-addr", "0.0.0.0:0")

	ready := false
	err := run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() error = nil, want a --http-addr validation error")
	}
	if ready {
		t.Fatal("run() called the ready callback, want no listener to start")
	}
	if !strings.Contains(err.Error(), "--http-addr") {
		t.Errorf("run() error = %q, want it to name --http-addr", err)
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("run() error = %v, want a usageError", err)
	}
	if strings.Contains(err.Error(), "connect") {
		t.Errorf("run() error = %q, want it to fail before connecting to the database", err)
	}
}

// TestRunCredentialErrorStopsBeforeDatabase asserts that a missing credential
// file is reported without touching the database or opening a listener.
func TestRunCredentialErrorStopsBeforeDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	args := runTestArgs(missing)

	ready := false
	err := run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() error = nil, want a credential-file error")
	}
	if ready {
		t.Fatal("run() called the ready callback, want no listener to start")
	}
	if !strings.Contains(err.Error(), "credential file") {
		t.Errorf("run() error = %q, want it to name the credential file", err)
	}
	var ue *usageError
	if errors.As(err, &ue) {
		t.Errorf("run() error = %v, want a credential error rather than a usage error", err)
	}
	if strings.Contains(err.Error(), "connect to database") {
		t.Errorf("run() error = %q, want it to fail before connecting to the database", err)
	}
}

// TestRunEmptyCredentialErrorStopsBeforeDatabase covers a file that exists but
// holds only whitespace.
func TestRunEmptyCredentialErrorStopsBeforeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-credential")
	if err := os.WriteFile(path, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	args := runTestArgs(path)

	ready := false
	err := run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() error = nil, want a credential-file error")
	}
	if ready {
		t.Fatal("run() called the ready callback, want no listener to start")
	}
	if !strings.Contains(err.Error(), "credential file") {
		t.Errorf("run() error = %q, want it to name the credential file", err)
	}
	if strings.Contains(err.Error(), "connect to database") {
		t.Errorf("run() error = %q, want it to fail before connecting to the database", err)
	}
}

// runTestLiveArgs is runTestArgs pointed at a real test database. Tests that
// get past the database connect use it.
func runTestLiveArgs(databaseURL, credentialFile string) []string {
	return []string{
		"--database-url", databaseURL,
		"--llm-credential-file", credentialFile,
		"--llm-credential-kind", "oauth_token",
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--shutdown-timeout", "1s",
	}
}

// TestRunControllerContextOutlivesSignal asserts that cancelling the run
// context, as a signal does, does not cancel controller.Run's context. Run must
// keep its in-flight provider calls alive until the HTTP server has stopped,
// which is when shutdown cancels it.
func TestRunControllerContextOutlivesSignal(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "run-context-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	var controllerCtx context.Context
	var checked atomic.Bool
	steps := &stepRecorder{}

	go func() {
		done <- runWithHooks(ctx, runTestLiveArgs(pool.Config().ConnString(), credentialFile), runHooks{
			controllerContext: func(c context.Context) { controllerCtx = c },
			ready:             func(string, string) { ready <- struct{}{} },
			shutdownStep: func(step string) {
				steps.record(step)
				if step != shutdownStepHTTP {
					return
				}
				checked.Store(true)
				if err := controllerCtx.Err(); err != nil {
					t.Errorf("controller.Run context was canceled before the HTTP step: %v", err)
				}
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before it listened: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("run did not become ready within 30s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v on shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after shutdown")
	}
	if !checked.Load() {
		t.Fatal("shutdown never reached the http step")
	}
}

// stepRecorder collects the shutdown steps the assembly reports.
type stepRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *stepRecorder) record(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *stepRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// TestRunUnexpectedServerExit closes one listener from under Serve and asserts
// that run shuts the remaining resources down in order and returns a non-zero
// error.
func TestRunUnexpectedServerExit(t *testing.T) {
	for _, target := range []string{"http", "grpc"} {
		t.Run(target, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			credentialFile := runTestCredentialFile(t, "unexpected-exit-token")
			steps := &stepRecorder{}
			ready := false

			err := runWithHooks(context.Background(), runTestLiveArgs(pool.Config().ConnString(), credentialFile), runHooks{
				ready:        func(string, string) { ready = true },
				shutdownStep: steps.record,
				listeners: func(httpLn, grpcLn net.Listener) {
					if target == "http" {
						_ = httpLn.Close()
					} else {
						_ = grpcLn.Close()
					}
				},
			})
			if err == nil {
				t.Fatalf("run() = nil after the %s server stopped, want a non-zero error", target)
			}
			if !ready {
				t.Error("run() did not call the ready callback before the server failure")
			}

			wantSteps := []string{
				shutdownStepHTTP,
				shutdownStepController,
				shutdownStepGRPC,
				shutdownStepProvider,
				shutdownStepPool,
			}
			gotSteps := steps.snapshot()
			if len(gotSteps) != len(wantSteps) {
				t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
			}
			for i := range wantSteps {
				if gotSteps[i] != wantSteps[i] {
					t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
				}
			}
		})
	}
}

// TestRunShutdownDeadlineLeavesProviderAndPool holds controller.Run open past
// the shutdown deadline. The plan requires step 2 to stop the gRPC server,
// skip the provider and the pool (closing them under in-flight provider calls
// would race), log an error and return a non-zero result.
func TestRunShutdownDeadlineLeavesProviderAndPool(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "stuck-controller-token")
	args := runTestLiveArgs(pool.Config().ConnString(), credentialFile)
	args = replaceFlag(t, args, "shutdown-timeout", "300ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	defer close(release)

	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	steps := &stepRecorder{}
	logStart := len(testLogs.String())

	go func() {
		done <- runWithHooks(ctx, args, runHooks{
			ready:        func(string, string) { ready <- struct{}{} },
			shutdownStep: steps.record,
			runController: func(runCtx context.Context) error {
				<-runCtx.Done()
				<-release
				return nil
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before it listened: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("run did not become ready within 30s")
	}

	start := time.Now()
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the controller shutdown deadline")
	}
	if err == nil {
		t.Fatal("run() = nil, want a non-zero result when controller.Run times out")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("run returned after %s, want it to wait for the 300ms shutdown deadline", elapsed)
	}

	// Step 3 (gRPC) still runs; only steps 4 and 5 (provider and pool) are
	// skipped when Run did not stop.
	wantSteps := []string{shutdownStepHTTP, shutdownStepController, shutdownStepGRPC}
	gotSteps := steps.snapshot()
	if len(gotSteps) != len(wantSteps) {
		t.Fatalf("shutdown steps = %v, want only %v; provider and pool must be left open", gotSteps, wantSteps)
	}
	for i := range wantSteps {
		if gotSteps[i] != wantSteps[i] {
			t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
		}
	}
	logs := logsSince(logStart)
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "controller did not stop") {
		t.Errorf("captured logs do not report the controller timeout at error level:\n%s", logs)
	}
}

// activeConnections returns a channel that fires when the HTTP server starts
// serving a connection, and the httpServer hook that feeds it. The hook is
// installed before the server starts accepting.
func activeConnections() (<-chan struct{}, func(*http.Server)) {
	active := make(chan struct{}, 1)
	return active, func(server *http.Server) {
		server.ConnState = func(_ net.Conn, state http.ConnState) {
			if state != http.StateActive {
				return
			}
			select {
			case active <- struct{}{}:
			default:
			}
		}
	}
}

// startHangingRequest dials the HTTP server, sends a complete head with an
// incomplete body and waits until the server starts serving the connection.
// The handler blocks in the body read, so the connection stays active until
// the server closes it.
func startHangingRequest(t *testing.T, httpAddr string, active <-chan struct{}) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatalf("dial the HTTP server: %v", err)
	}
	if _, err := fmt.Fprintf(conn,
		"POST /v1/runs HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{",
		httpAddr); err != nil {
		_ = conn.Close()
		t.Fatalf("send the request head: %v", err)
	}
	select {
	case <-active:
	case <-time.After(10 * time.Second):
		_ = conn.Close()
		t.Fatal("the HTTP server did not start serving the request")
	}
	return conn
}

// TestRunUnexpectedExitStopsEstablishedConnection closes the HTTP listener
// while a request is in flight. Serve stops accepting but leaves the accepted
// connection open, so shutdown must still stop the server: otherwise the
// request survives the controller step and can keep calling the controller
// after the provider and the pool are released.
//
// The hanging request pins the HTTP step to the shutdown deadline, so the
// later steps have no budget left and must report that the deadline was
// exhausted rather than racing a zero timer.
func TestRunUnexpectedExitStopsEstablishedConnection(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "established-connection-token")

	release := make(chan struct{})
	defer close(release)

	active, connStateHook := activeConnections()
	var requestConn net.Conn
	t.Cleanup(func() {
		if requestConn != nil {
			_ = requestConn.Close()
		}
	})

	steps := &stepRecorder{}
	logStart := len(testLogs.String())
	ended := false

	err := runWithHooks(context.Background(), runTestLiveArgs(pool.Config().ConnString(), credentialFile), runHooks{
		httpServer: connStateHook,
		ready: func(httpAddr, _ string) {
			requestConn = startHangingRequest(t, httpAddr, active)
		},
		listeners: func(httpLn, _ net.Listener) {
			_ = httpLn.Close()
		},
		runController: func(ctx context.Context) error {
			<-ctx.Done()
			<-release
			return nil
		},
		shutdownStep: func(step string) {
			steps.record(step)
			if step != shutdownStepController {
				return
			}
			// The HTTP step must already have ended the in-flight request. A
			// timeout here means the connection is still open.
			_ = requestConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, readErr := requestConn.Read(make([]byte, 1)); readErr == nil {
				t.Error("the established connection produced data instead of closing")
				return
			} else if isTimeout(readErr) {
				t.Error("the established connection is still open at the controller shutdown step")
				return
			}
			ended = true
		},
	})
	if err == nil {
		t.Fatal("run() = nil after an unexpected HTTP exit, want a non-zero error")
	}
	if !ended {
		t.Fatal("the established connection was not ended before the controller step")
	}

	// The hanging request consumed the whole shutdown deadline, so the
	// controller wait and the provider and pool closes are skipped.
	wantSteps := []string{shutdownStepHTTP, shutdownStepController, shutdownStepGRPC}
	gotSteps := steps.snapshot()
	if len(gotSteps) != len(wantSteps) {
		t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
	}
	for i := range wantSteps {
		if gotSteps[i] != wantSteps[i] {
			t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
		}
	}
	logs := logsSince(logStart)
	if !strings.Contains(logs, "deadline exhausted") {
		t.Errorf("captured logs do not report the exhausted shutdown deadline:\n%s", logs)
	}
}

// isTimeout reports whether err is a net.Error timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestRunDeadlineExhaustedSkipsProviderAndPool keeps the HTTP step at the
// shutdown deadline with a hanging request while controller.Run has already
// returned. The provider and the pool then have no remaining budget; shutdown
// records that the deadline was exhausted and skips both, instead of racing a
// zero-length timer.
func TestRunDeadlineExhaustedSkipsProviderAndPool(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "exhausted-deadline-token")

	active, connStateHook := activeConnections()
	var requestConn net.Conn
	t.Cleanup(func() {
		if requestConn != nil {
			_ = requestConn.Close()
		}
	})

	controllerReturned := make(chan struct{})
	steps := &stepRecorder{}
	logStart := len(testLogs.String())

	err := runWithHooks(context.Background(), runTestLiveArgs(pool.Config().ConnString(), credentialFile), runHooks{
		httpServer: connStateHook,
		ready: func(httpAddr, _ string) {
			// The controller must be done before shutdown starts, so step 2
			// takes its result without waiting and steps 4 and 5 see a zero
			// budget.
			<-controllerReturned
			requestConn = startHangingRequest(t, httpAddr, active)
		},
		listeners: func(httpLn, _ net.Listener) {
			_ = httpLn.Close()
		},
		runController: func(context.Context) error {
			close(controllerReturned)
			return nil
		},
		shutdownStep: steps.record,
	})
	if err == nil {
		t.Fatal("run() = nil after an unexpected HTTP exit, want the server error")
	}

	wantSteps := []string{
		shutdownStepHTTP,
		shutdownStepController,
		shutdownStepGRPC,
		shutdownStepProvider,
		shutdownStepPool,
	}
	gotSteps := steps.snapshot()
	if len(gotSteps) != len(wantSteps) {
		t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
	}
	for i := range wantSteps {
		if gotSteps[i] != wantSteps[i] {
			t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
		}
	}

	logs := logsSince(logStart)
	for _, step := range []string{shutdownStepProvider, shutdownStepPool} {
		if !strings.Contains(logs, "deadline exhausted") || !strings.Contains(logs, "step="+step) {
			t.Errorf("captured logs do not report the exhausted deadline for %s:\n%s", step, logs)
		}
	}
}

// TestRunStartupSignalReturnsCleanly asserts that a signal arriving during
// startup (here before the database is reached) is a normal exit: run returns
// nil so main exits 0, and no listener is opened.
func TestRunStartupSignalReturnsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	credentialFile := runTestCredentialFile(t, "startup-signal-token")
	ready := false
	err := run(ctx, runTestArgs(credentialFile), func(string, string) { ready = true })
	if err != nil {
		t.Fatalf("run() = %v, want nil after a signal during startup", err)
	}
	if ready {
		t.Fatal("run() called the ready callback after a startup signal")
	}
}

// TestRunProviderConstructionSignalReturnsCleanly asserts that a signal that
// cancels the context while the sandbox provider is being constructed is a
// clean signal exit. gRPC reports the cancellation as a status error rather
// than a context.Canceled error, so startup must recognize both forms.
func TestRunProviderConstructionSignalReturnsCleanly(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "provider-signal-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := false
	err := runWithHooks(ctx, runTestLiveArgs(pool.Config().ConnString(), credentialFile), runHooks{
		newProvider: func(ctx context.Context, _ *Config) (sandboxprovider.Provider, error) {
			// The signal arrives while the provider connects to ate-api.
			cancel()
			<-ctx.Done()
			return nil, fmt.Errorf("substrate: connect to ate-api: %w",
				status.Error(codes.Canceled, "context canceled"))
		},
		ready: func(string, string) { ready = true },
	})
	if err != nil {
		t.Fatalf("run() = %v, want nil after a signal during provider construction", err)
	}
	if ready {
		t.Fatal("run() opened listeners after the provider construction was canceled")
	}
}

// runTestNonLoopbackAddr returns a bindable non-loopback IP literal with a
// free port. Substrate mode validates --grpc-addr that way, because the
// address is advertised to actors; local mode does not.
func runTestNonLoopbackAddr(t *testing.T) string {
	t.Helper()
	// Dialing UDP selects a source address without sending anything.
	conn, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		t.Fatalf("select a non-loopback local address: %v", err)
	}
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	_ = conn.Close()
	if ip.IsLoopback() || ip.IsUnspecified() {
		t.Skip("no non-loopback local address is available")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatalf("find a free port on %s: %v", ip, err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestRunSubstrateConstructionFailure runs the real startup path against a
// usable database and a kubeconfig that does not exist. The provider must be
// constructed before any listener opens: run returns the provider error, the
// ready callback is never called, and the occupied HTTP address is never
// reached. Moving newProvider after the listeners turns the error into a
// listen error and makes this test fail.
func TestRunSubstrateConstructionFailure(t *testing.T) {
	pool := pgtest.NewPool(t)
	credentialFile := runTestCredentialFile(t, "substrate-construction-token")
	kubeconfig := filepath.Join(t.TempDir(), "does-not-exist-kubeconfig")
	grpcAddr := runTestNonLoopbackAddr(t)

	// Hold a loopback port so a listen before provider construction is
	// visible as a different error.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a loopback port: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	args := []string{
		"--database-url", pool.Config().ConnString(),
		"--llm-credential-file", credentialFile,
		"--llm-credential-kind", "oauth_token",
		"--provider", "substrate",
		"--substrate-kubeconfig", kubeconfig,
		"--substrate-image", "registry.example/actor@sha256:" + strings.Repeat("a", 64),
		"--http-addr", occupied.Addr().String(),
		"--grpc-addr", grpcAddr,
		"--shutdown-timeout", "1s",
	}

	ready := false
	err = run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() = nil, want the substrate provider construction error")
	}
	if ready {
		t.Fatal("run() opened listeners before the sandbox provider was constructed")
	}
	if !strings.Contains(err.Error(), "substrate") {
		t.Errorf("run() error = %q, want it to name the substrate provider", err)
	}
	if strings.Contains(err.Error(), "--http-addr") {
		t.Errorf("run() error = %q, want the provider error before any listen", err)
	}
}

// replaceFlag returns args with the value of name replaced.
func replaceFlag(t *testing.T, args []string, name, value string) []string {
	t.Helper()
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name {
			if i+1 >= len(args) {
				t.Fatalf("flag --%s has no value in %q", name, args)
			}
			out = append(out, args[i], value)
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
