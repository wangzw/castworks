package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/humanchannel/api"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

// smokeShutdownTimeout is the --shutdown-timeout the smoke tests pass. The
// gRPC stream they hold open never ends, so shutdown must rely on the
// deadline.
const smokeShutdownTimeout = 2 * time.Second

// smokeStopMargin is how long a test waits beyond the shutdown timeout before
// calling the process stuck.
const smokeStopMargin = 8 * time.Second

// databasePassword returns the password embedded in a PostgreSQL URL, or the
// empty string when the URL carries none.
func databasePassword(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	if u.User == nil {
		return ""
	}
	password, ok := u.User.Password()
	if !ok {
		return ""
	}
	return password
}

// smokeControlPlane is a control plane started in-process by runWithHooks.
type smokeControlPlane struct {
	httpAddr string
	grpcAddr string
	steps    *stepRecorder
	// stepHook runs after the recorder for each shutdown step. It is stored
	// atomically because the serve goroutine reads it.
	stepHook atomic.Pointer[func(step string)]

	cancel   context.CancelFunc
	errCh    chan error
	stopOnce sync.Once
	stopErr  error
}

// onStep installs a callback that runs for every shutdown step, after the step
// is recorded. It must be called before stop.
func (cp *smokeControlPlane) onStep(fn func(step string)) {
	cp.stepHook.Store(&fn)
}

// stop cancels the run context and waits for run to return. It is safe to call
// more than once; later calls return the first result.
func (cp *smokeControlPlane) stop(t *testing.T) error {
	t.Helper()
	cp.stopOnce.Do(func() {
		cp.cancel()
		select {
		case cp.stopErr = <-cp.errCh:
		case <-time.After(smokeShutdownTimeout + smokeStopMargin):
			cp.stopErr = errors.New("run did not return after the shutdown deadline")
		}
	})
	return cp.stopErr
}

// startSmoke starts the control plane on a fresh database with --provider
// local. The credential file holds token with a trailing newline.
//
// The cleanup that stops run is registered after pgtest.NewPool's, so it runs
// first and the database is not dropped while the process still holds it. The
// credential and database-password leak checks are registered between the two
// so they see the shutdown logs.
func startSmoke(t *testing.T, token string) *smokeControlPlane {
	t.Helper()

	pool := pgtest.NewPool(t)
	dbPassword := databasePassword(t, pool.Config().ConnString())
	credentialFile := filepath.Join(t.TempDir(), "llm-credential")
	if err := os.WriteFile(credentialFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}

	args := []string{
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--database-url", pool.Config().ConnString(),
		"--llm-credential-file", credentialFile,
		"--llm-credential-kind", "oauth_token",
		"--provider", "local",
		"--shutdown-timeout", smokeShutdownTimeout.String(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cp := &smokeControlPlane{
		cancel: cancel,
		errCh:  make(chan error, 1),
		steps:  &stepRecorder{},
	}
	ready := make(chan [2]string, 1)

	go func() {
		cp.errCh <- runWithHooks(ctx, args, runHooks{
			ready: func(httpAddr, grpcAddr string) {
				ready <- [2]string{httpAddr, grpcAddr}
			},
			shutdownStep: func(step string) {
				cp.steps.record(step)
				if fn := cp.stepHook.Load(); fn != nil {
					(*fn)(step)
				}
			},
		})
	}()

	select {
	case addrs := <-ready:
		cp.httpAddr, cp.grpcAddr = addrs[0], addrs[1]
	case err := <-cp.errCh:
		cancel()
		t.Fatalf("run exited before it listened: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("run did not become ready within 30s")
	}

	// Registered after pgtest.NewPool and before stop, so it runs after stop
	// and before the database is dropped.
	t.Cleanup(func() {
		text := testLogs.String()
		if strings.Contains(text, token) {
			t.Errorf("captured slog output contains the credential token")
		}
		if dbPassword != "" && strings.Contains(text, dbPassword) {
			t.Errorf("captured slog output contains the database password")
		}
	})
	t.Cleanup(func() {
		if err := cp.stop(t); err != nil && !t.Failed() {
			t.Errorf("run returned %v on shutdown, want nil", err)
		}
	})

	return cp
}

// doJSON performs one HTTP request against the control plane and returns the
// response. The caller closes the body.
func doJSON(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// createRun creates a run over HTTP and returns its id.
func createRun(t *testing.T, cp *smokeControlPlane, prompt string) string {
	t.Helper()
	body, err := json.Marshal(api.CreateRunRequest{Prompt: prompt})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}
	resp := doJSON(t, http.MethodPost, "http://"+cp.httpAddr+"/v1/runs", body)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/runs status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, raw)
	}
	var created api.CreateRunResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create response %q: %v", raw, err)
	}
	if created.RunID == "" {
		t.Fatalf("POST /v1/runs returned an empty run_id; body: %s", raw)
	}
	return created.RunID
}

// getRun fetches one run's detail over HTTP.
func getRun(t *testing.T, cp *smokeControlPlane, runID string) api.RunDetail {
	t.Helper()
	resp := doJSON(t, http.MethodGet, "http://"+cp.httpAddr+"/v1/runs/"+runID, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read get response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s status = %d, want %d; body: %s", runID, resp.StatusCode, http.StatusOK, raw)
	}
	var detail api.RunDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode run detail %q: %v", raw, err)
	}
	return detail
}

// listRuns fetches the run list over HTTP.
func listRuns(t *testing.T, cp *smokeControlPlane) []api.RunSummary {
	t.Helper()
	resp := doJSON(t, http.MethodGet, "http://"+cp.httpAddr+"/v1/runs", nil)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read list response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/runs status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, raw)
	}
	var runs []api.RunSummary
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatalf("decode run list %q: %v", raw, err)
	}
	return runs
}

// dialGRPC opens a client connection to the control plane.
func dialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// openStream opens an AgentStream and sends the Hello for runID. The caller
// reads the HelloAck.
func openStream(t *testing.T, conn *grpc.ClientConn, runID string) grpc.BidiStreamingClient[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame] {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	stream, err := agentprotocolv1.NewAgentStreamClient(conn).Connect(ctx, grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("connect agent stream: %v", err)
	}
	frame := &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Hello{
			Hello: &agentprotocolv1.Hello{
				AgentState: &agentprotocolv1.AgentState{Phase: agentprotocolv1.Phase_PHASE_STARTING},
			},
		},
	}
	if err := stream.Send(frame); err != nil {
		t.Fatalf("send Hello for %q: %v", runID, err)
	}
	return stream
}

// TestSmokeCreateQueryAndGRPC covers the full wiring: HTTP creation and
// queries prove Migrate, runcontroller and humanchannel are connected, and the
// gRPC Hello proves AgentStream answers with the credential read at startup.
func TestSmokeCreateQueryAndGRPC(t *testing.T) {
	token := fmt.Sprintf("smoke-create-token-%d", time.Now().UnixNano())
	cp := startSmoke(t, token)

	runID := createRun(t, cp, "hello smoke")

	detail := getRun(t, cp, runID)
	if detail.State != string(runcontroller.StateProvisioning) {
		t.Errorf("GET /v1/runs/%s state = %q, want %q", runID, detail.State, runcontroller.StateProvisioning)
	}

	found := false
	for _, run := range listRuns(t, cp) {
		if run.RunID == runID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GET /v1/runs does not contain %s", runID)
	}

	conn := dialGRPC(t, cp.grpcAddr)
	stream := openStream(t, conn, runID)
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive HelloAck: %v", err)
	}
	helloAck := ack.GetHelloAck()
	if helloAck == nil {
		t.Fatalf("first control frame = %v, want HelloAck", ack)
	}
	credential := helloAck.GetLlmCredential()
	if credential.GetKind() != agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_OAUTH_TOKEN {
		t.Errorf("credential kind = %v, want LLM_CREDENTIAL_KIND_OAUTH_TOKEN", credential.GetKind())
	}
	if credential.GetValue() != token {
		t.Errorf("credential value = %q, want the trimmed file contents", credential.GetValue())
	}

	// A Hello for an unknown run must close the stream with NotFound.
	const missingRunID = "zzzzzzzzzzzz"
	missing := openStream(t, conn, missingRunID)
	if _, err := missing.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("Hello for an unknown run returned %v, want NotFound", err)
	}
}

// TestSmokeGracefulShutdown covers the shutdown order, the deadline that
// unblocks GracefulStop on a long-lived stream, and the closed listeners.
func TestSmokeGracefulShutdown(t *testing.T) {
	token := fmt.Sprintf("smoke-shutdown-token-%d", time.Now().UnixNano())
	cp := startSmoke(t, token)

	runID := createRun(t, cp, "keep the stream open")
	conn := dialGRPC(t, cp.grpcAddr)
	stream := openStream(t, conn, runID)
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("receive HelloAck: %v", err)
	}

	logStart := len(testLogs.String())
	// At the HTTP step the HTTP listener must already refuse connections while
	// gRPC still accepts them: the recorded order has to match the real order.
	cp.onStep(func(step string) {
		if step != shutdownStepHTTP {
			return
		}
		if conn, err := net.DialTimeout("tcp", cp.httpAddr, time.Second); err == nil {
			_ = conn.Close()
			t.Errorf("HTTP still accepts connections at the %s shutdown step", shutdownStepHTTP)
		}
		conn, err := net.DialTimeout("tcp", cp.grpcAddr, time.Second)
		if err != nil {
			t.Errorf("gRPC refuses connections at the %s shutdown step: %v", shutdownStepHTTP, err)
			return
		}
		_ = conn.Close()
	})

	start := time.Now()
	err := cp.stop(t)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run returned %v on shutdown, want nil", err)
	}
	if elapsed > smokeShutdownTimeout+5*time.Second {
		t.Errorf("shutdown took %s, want less than %s", elapsed, smokeShutdownTimeout+5*time.Second)
	}

	shutdownLogs := logsSince(logStart)
	if !strings.Contains(shutdownLogs, "controller stopped") {
		t.Errorf("shutdown logs do not report controller stopped:\n%s", shutdownLogs)
	}
	if strings.Contains(shutdownLogs, "controller did not stop") {
		t.Errorf("shutdown logs report that the controller did not stop:\n%s", shutdownLogs)
	}

	wantSteps := []string{
		shutdownStepHTTP,
		shutdownStepController,
		shutdownStepGRPC,
		shutdownStepProvider,
		shutdownStepPool,
	}
	gotSteps := cp.steps.snapshot()
	if len(gotSteps) != len(wantSteps) {
		t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
	}
	for i := range wantSteps {
		if gotSteps[i] != wantSteps[i] {
			t.Fatalf("shutdown steps = %v, want %v", gotSteps, wantSteps)
		}
	}

	for _, addr := range []string{cp.httpAddr, cp.grpcAddr} {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Errorf("address %s still accepts connections after shutdown", addr)
		}
	}
}

// TestSmokeHTTPServesOnlyOnLoopback checks the listening log and the rejection
// of a wildcard address before any other resource is touched.
func TestSmokeHTTPServesOnlyOnLoopback(t *testing.T) {
	token := fmt.Sprintf("smoke-loopback-token-%d", time.Now().UnixNano())
	cp := startSmoke(t, token)

	host, _, err := net.SplitHostPort(cp.httpAddr)
	if err != nil {
		t.Fatalf("split HTTP address %q: %v", cp.httpAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("HTTP address host %q is not a loopback IP", host)
	}
	if !strings.Contains(testLogs.String(), "http listening") ||
		!strings.Contains(testLogs.String(), cp.httpAddr) {
		t.Errorf("captured logs do not report http listening on %s", cp.httpAddr)
	}
	if !strings.Contains(testLogs.String(), "grpc listening") ||
		!strings.Contains(testLogs.String(), cp.grpcAddr) {
		t.Errorf("captured logs do not report grpc listening on %s", cp.grpcAddr)
	}

	// A wildcard HTTP address must fail before any listener or database use.
	credentialFile := runTestCredentialFile(t, token)
	args := replaceFlag(t, runTestArgs(credentialFile), "http-addr", "0.0.0.0:0")

	ready := false
	err = run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() error = nil, want a --http-addr error")
	}
	if ready {
		t.Fatal("run() called the ready callback for a wildcard HTTP address")
	}
	if !strings.Contains(err.Error(), "--http-addr") {
		t.Errorf("run() error = %q, want it to name --http-addr", err)
	}
	if strings.Contains(err.Error(), "connect") {
		t.Errorf("run() error = %q, want it to fail before touching the database", err)
	}
}

// TestSmokeStartupFailuresAreClean covers a credential file with no value and
// an unreachable database. Neither may open a listener, and the database error
// may not echo the password from the URL.
func TestSmokeStartupFailuresAreClean(t *testing.T) {
	token := fmt.Sprintf("smoke-failure-token-%d", time.Now().UnixNano())
	password := fmt.Sprintf("smoke-failure-secret-%d", time.Now().UnixNano())

	t.Run("empty credential", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty-credential")
		if err := os.WriteFile(path, []byte("\n\t \n"), 0o600); err != nil {
			t.Fatalf("write credential file: %v", err)
		}
		args := runTestArgs(path)

		ready := false
		err := run(context.Background(), args, func(string, string) { ready = true })
		if err == nil {
			t.Fatal("run() error = nil, want a credential-file error")
		}
		if ready {
			t.Fatal("run() called the ready callback, want no listener")
		}
		if !strings.Contains(err.Error(), "credential file") {
			t.Errorf("run() error = %q, want it to name the credential file", err)
		}
	})

	t.Run("unreachable database", func(t *testing.T) {
		credentialFile := runTestCredentialFile(t, token)
		databaseURL := "postgres://castworks:" + password + "@127.0.0.1:1/castworks?sslmode=disable&connect_timeout=1"
		args := replaceFlag(t, runTestArgs(credentialFile), "database-url", databaseURL)

		ready := false
		err := run(context.Background(), args, func(string, string) { ready = true })
		if err == nil {
			t.Fatal("run() error = nil, want a connection error")
		}
		if ready {
			t.Fatal("run() called the ready callback, want no listener")
		}
		if !strings.Contains(err.Error(), "connect to database") {
			t.Errorf("run() error = %q, want it to report the database connection", err)
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("run() error = %q, must not contain the database password", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("run() error = %q, must not contain the credential token", err)
		}
	})
}
