package main

// In-process integration tests for the connectorhub module. A control plane is
// started through runWithHooks on a real PostgreSQL database, with the fake
// sandbox provider as newProvider and a fake warden process for every run. The
// extsim binary is built into a temporary directory and run as a child process;
// its test-control interface sets job plans and reports callback deliveries.
//
// The five requirements that the milestone document marks for the real extsim
// are callback-before-registration, dropped-callback verification, delivery
// resend and duplicate, cancellation after the run ends, and delivery of a
// leftover pending record after a restart. Two more requirements use real
// extsim: the concurrency cases and the retry after a database error during
// the first match. The two failure-injection requirements (database error on
// the first match and a temporarily failing AppendInbox) use PostgreSQL
// triggers in the test database instead of a wrapped Runs, so the production
// assembly stays unchanged.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	"github.com/wangzw/castworks/internal/testutil/pgtest"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

const (
	// chHTTPTimeout bounds one request the tests make to the control plane or
	// to extsim.
	chHTTPTimeout = 15 * time.Second
	// chShutdownTimeout is the control plane's --shutdown-timeout. The fake
	// wardens are stopped before the plane, so shutdown normally finishes
	// faster than this.
	chShutdownTimeout = 5 * time.Second
	// chWaitTimeout bounds every asynchronous assertion. It is a stuck test
	// detector, not a latency budget.
	chWaitTimeout = 30 * time.Second
	// chExtsimWaitTimeout bounds building and starting the extsim child.
	chExtsimWaitTimeout = 30 * time.Second
	// chWebhookBindAttempts bounds how often startPlane re-reserves the
	// webhook address when the control plane cannot bind it.
	chWebhookBindAttempts = 5
)

// TestConnectorhubIntegration runs the process-internal connectorhub tests
// required by docs/milestone/v0.2.0/connectorhub.md "测试".
func TestConnectorhubIntegration(t *testing.T) {
	bin := chBuildExtsim(t)

	t.Run("callback before registration", func(t *testing.T) {
		testConnectorhubCallbackBeforeRegistration(t, bin)
	})
	t.Run("dropped callback verified", func(t *testing.T) {
		testConnectorhubDroppedCallbackVerified(t, bin)
	})
	t.Run("concurrent registration", func(t *testing.T) {
		testConnectorhubConcurrentRegistration(t, bin)
	})
	t.Run("duplicate delivery", func(t *testing.T) {
		testConnectorhubDuplicateDelivery(t, bin)
	})
	t.Run("database error during first match", func(t *testing.T) {
		testConnectorhubMatchDatabaseError(t, bin)
	})
	t.Run("append inbox retry", func(t *testing.T) {
		testConnectorhubAppendInboxRetry(t, bin)
	})
	t.Run("delivery after restart", func(t *testing.T) {
		testConnectorhubDeliveryAfterRestart(t, bin)
	})
	t.Run("cancel after run end", func(t *testing.T) {
		testConnectorhubCancelAfterRunEnd(t, bin)
	})
	t.Run("subscription parameter validation", func(t *testing.T) {
		testConnectorhubSubscriptionValidation(t, bin)
	})
	t.Run("webhook rejection", func(t *testing.T) {
		testConnectorhubWebhookRejection(t, bin)
	})
}

// chBuildExtsim builds cmd/extsim into a temporary directory once per test.
func chBuildExtsim(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "extsim")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/extsim")
	cmd.Dir = chModuleRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/extsim: %v\n%s", err, output)
	}
	return out
}

// chModuleRoot walks up from the working directory to the module root.
func chModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// chAcquireLoopbackAddr reserves an ephemeral loopback port and releases it, so
// a listener started later can bind the same address. The restart test uses it
// to keep the webhook address stable across two control plane processes.
func chAcquireLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved loopback port: %v", err)
	}
	return addr
}

// connectorhubEnv is a shared test database plus one extsim child process.
// Control planes can be started against it repeatedly, which the restart test
// needs.
type connectorhubEnv struct {
	t              *testing.T
	pool           *pgxpool.Pool
	extsim         *connectorhubExtsim
	key            []byte // signing key without the trailing newline
	keyFile        string
	credFile       string
	bin            string
	definitionPath string
	// configPath points at the connectorhub YAML loaded by every plane.
	configPath string
	// webhookAddr is the pre-reserved address extsim calls back to.
	webhookAddr string
	// started records that one plane has started successfully. After that the
	// webhook address is fixed, which the restart test needs for both planes.
	started bool
}

// newConnectorhubEnv creates an isolated database, a signing key, a credential
// file and an extsim process, then writes the connectorhub configuration.
func newConnectorhubEnv(t *testing.T, bin string) *connectorhubEnv {
	t.Helper()

	pool := pgtest.NewPool(t)
	dir := t.TempDir()

	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	key := []byte(hex.EncodeToString(rawKey))
	keyFile := filepath.Join(dir, "extsim-signing-key")
	if err := os.WriteFile(keyFile, append(append([]byte(nil), key...), '\n'), 0o600); err != nil {
		t.Fatalf("write signing key: %v", err)
	}

	credFile := filepath.Join(dir, "llm-credential")
	if err := os.WriteFile(credFile, []byte("connectorhub-test-token\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}

	webhookAddr := chAcquireLoopbackAddr(t)
	ext := chStartExtsim(t, bin, "http://"+webhookAddr+"/hooks/extsim", keyFile)

	env := &connectorhubEnv{
		t:              t,
		pool:           pool,
		extsim:         ext,
		key:            key,
		keyFile:        keyFile,
		credFile:       credFile,
		bin:            bin,
		definitionPath: filepath.Join(chModuleRoot(t), "deploy", "dev", "connectorhub", "extsim.yaml"),
		configPath:     filepath.Join(dir, "connectorhub.yaml"),
		webhookAddr:    webhookAddr,
	}
	env.writeConfig()
	return env
}

// writeConfig writes the connectorhub YAML for the current extsim and webhook
// addresses. startPlane rewrites it when it restarts extsim on a new address.
func (e *connectorhubEnv) writeConfig() {
	e.t.Helper()
	config := fmt.Sprintf(`project: default
systems:
  - definition: %s
    signing_key_file: %s
    api_base_url: http://%s
bindings:
  extsim: [demo]
`, e.definitionPath, e.keyFile, e.extsim.jobsAddr)
	if err := os.WriteFile(e.configPath, []byte(config), 0o600); err != nil {
		e.t.Fatalf("write connectorhub config: %v", err)
	}
}

// connectorhubPlane is one control plane running through runWithHooks.
type connectorhubPlane struct {
	t *testing.T

	httpAddr      string
	grpcAddr      string
	connectorAddr string
	webhookAddr   string

	mu      sync.Mutex
	wardens []*fakewarden.Warden

	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
	stopErr  error
}

// startPlane starts a control plane for webhookAddr. Every run the fake
// provider provisions gets a fake warden connected to this plane.
//
// The webhook address is reserved before extsim starts, so a listener that
// grabs the port in between makes the plane fail with "address already in
// use". Until a plane has started successfully, that failure is treated as a
// lost race: startPlane reserves another address, restarts extsim against it
// and tries again, keeping every failed attempt in the final error. Once a
// plane has started, the address stays fixed because the restart test needs
// both planes on the same one.
func (e *connectorhubEnv) startPlane(t *testing.T, webhookAddr string) *connectorhubPlane {
	t.Helper()

	if e.started {
		p, err := e.launchPlane(t, webhookAddr)
		if err != nil {
			t.Fatalf("control plane exited before it listened: %v", err)
		}
		return p
	}

	var conflicts []string
	for attempt := 1; attempt <= chWebhookBindAttempts; attempt++ {
		p, err := e.launchPlane(t, e.webhookAddr)
		if err == nil {
			e.started = true
			return p
		}
		if !chWebhookBindConflict(err) {
			t.Fatalf("control plane exited before it listened: %v", err)
		}
		conflicts = append(conflicts, fmt.Sprintf("attempt %d on %s: %v", attempt, e.webhookAddr, err))
		t.Logf("webhook address %s was taken during startup (attempt %d)", e.webhookAddr, attempt)
		if attempt == chWebhookBindAttempts {
			break
		}
		e.rebindExtsim(t)
	}
	t.Fatalf("control plane could not bind a reserved webhook address after %d attempts:\n%s",
		chWebhookBindAttempts, strings.Join(conflicts, "\n"))
	return nil
}

// chWebhookBindConflict reports whether err is the control plane failing to
// bind its webhook listener because the address is already in use.
func chWebhookBindConflict(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) && strings.Contains(err.Error(), "--webhook-addr")
}

// rebindExtsim stops extsim, reserves a new webhook address and starts a fresh
// extsim that calls back to it. startPlane calls it only before any plane has
// started, so no job can be in flight; rewriting the config is what makes the
// next plane reach the new extsim.
func (e *connectorhubEnv) rebindExtsim(t *testing.T) {
	t.Helper()

	e.extsim.stop()
	e.webhookAddr = chAcquireLoopbackAddr(t)
	e.extsim = chStartExtsim(t, e.bin, "http://"+e.webhookAddr+"/hooks/extsim", e.keyFile)
	e.writeConfig()
}

// launchPlane makes one attempt at starting the control plane. It returns the
// startup error instead of failing the test so a bind conflict can be retried;
// it fails the test directly when the plane never becomes ready.
func (e *connectorhubEnv) launchPlane(t *testing.T, webhookAddr string) (*connectorhubPlane, error) {
	t.Helper()

	args := []string{
		"--database-url", e.pool.Config().ConnString(),
		"--llm-credential-file", e.credFile,
		"--llm-credential-kind", "oauth_token",
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--connectorhub-config", e.configPath,
		"--connector-addr", "127.0.0.1:0",
		"--webhook-addr", webhookAddr,
		"--shutdown-timeout", chShutdownTimeout.String(),
	}

	provider := fakeprovider.New(t)
	p := &connectorhubPlane{t: t, webhookAddr: webhookAddr}
	provider.OnProvisioned(p.startWarden)

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan error, 1)

	readyCh := make(chan [2]string, 1)
	connectorCh := make(chan [2]string, 1)
	go func() {
		p.done <- runWithHooks(ctx, args, runHooks{
			ready: func(httpAddr, grpcAddr string) {
				readyCh <- [2]string{httpAddr, grpcAddr}
			},
			connectorReady: func(connectorAddr, webhookAddr string) {
				connectorCh <- [2]string{connectorAddr, webhookAddr}
			},
			newProvider: func(context.Context, *Config) (sandboxprovider.Provider, error) {
				return provider, nil
			},
		})
	}()

	deadline := time.After(chWaitTimeout)
	for p.httpAddr == "" || p.connectorAddr == "" {
		select {
		case addrs := <-readyCh:
			p.httpAddr, p.grpcAddr = addrs[0], addrs[1]
		case addrs := <-connectorCh:
			p.connectorAddr, p.webhookAddr = addrs[0], addrs[1]
		case err := <-p.done:
			cancel()
			return nil, err
		case <-deadline:
			cancel()
			t.Fatalf("control plane did not become ready within %s", chWaitTimeout)
			return nil, nil
		}
	}

	t.Cleanup(func() {
		if err := p.stop(); err != nil && !t.Failed() {
			t.Errorf("control plane shutdown returned %v, want nil", err)
		}
	})
	return p, nil
}

// startWarden starts a fake warden for a provisioned run.
func (p *connectorhubPlane) startWarden(runID string) {
	p.mu.Lock()
	addr := p.grpcAddr
	p.mu.Unlock()

	w := fakewarden.New(p.t, runID, func(context.Context) (*grpc.ClientConn, error) {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	})
	p.mu.Lock()
	p.wardens = append(p.wardens, w)
	p.mu.Unlock()
	w.Start()
}

// stop closes the fake wardens, cancels the run context and waits for
// runWithHooks to return. It is safe to call more than once.
func (p *connectorhubPlane) stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		wardens := append([]*fakewarden.Warden(nil), p.wardens...)
		p.mu.Unlock()
		for _, w := range wardens {
			w.Stop()
		}
		p.cancel()
		select {
		case p.stopErr = <-p.done:
		case <-time.After(chShutdownTimeout + 15*time.Second):
			p.stopErr = errors.New("run returned too late after shutdown")
		}
	})
	return p.stopErr
}

// createRun creates a run over the control plane HTTP API and returns its id.
func (p *connectorhubPlane) createRun() string {
	p.t.Helper()
	body, err := json.Marshal(map[string]string{"prompt": "connectorhub integration run"})
	if err != nil {
		p.t.Fatalf("marshal create request: %v", err)
	}
	status, data, err := chHTTPDo(http.MethodPost, "http://"+p.httpAddr+"/v1/runs", body,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		p.t.Fatalf("POST /v1/runs: %v", err)
	}
	if status != http.StatusCreated {
		p.t.Fatalf("POST /v1/runs status = %d, want %d; body: %s", status, http.StatusCreated, data)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.RunID == "" {
		p.t.Fatalf("decode create response %q: %v", data, err)
	}
	return created.RunID
}

// endRun ends a run over the control plane HTTP API.
func (p *connectorhubPlane) endRun(runID string) {
	p.t.Helper()
	status, data, err := chHTTPDo(http.MethodPost,
		"http://"+p.httpAddr+"/v1/runs/"+runID+"/end", []byte("{}"),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		p.t.Fatalf("POST /v1/runs/%s/end: %v", runID, err)
	}
	if status != http.StatusOK {
		p.t.Fatalf("POST /v1/runs/%s/end status = %d, want %d; body: %s",
			runID, status, http.StatusOK, data)
	}
}

// chSubscribeRequest is the connector endpoint's subscription body.
type chSubscribeRequest struct {
	System      string            `json:"system"`
	Event       string            `json:"event"`
	Correlation map[string]string `json:"correlation"`
}

// chSubscribeResponse is the connector endpoint's 201 body.
type chSubscribeResponse struct {
	SubID       string `json:"sub_id"`
	SinceCursor int64  `json:"since_cursor"`
	Status      string `json:"status"`
}

// postSubscription sends a subscription request and returns the status and raw
// body.
func (p *connectorhubPlane) postSubscription(runID string, req chSubscribeRequest) (int, []byte) {
	p.t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		p.t.Fatalf("marshal subscription request: %v", err)
	}
	status, data, err := chHTTPDo(http.MethodPost,
		"http://"+p.connectorAddr+"/v1/runs/"+runID+"/subscriptions", body,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		p.t.Fatalf("POST subscription for %s: %v", runID, err)
	}
	return status, data
}

// subscribeOK registers a subscription and requires a 201 response.
func (p *connectorhubPlane) subscribeOK(runID, event string, correlation map[string]string) chSubscribeResponse {
	p.t.Helper()
	status, data := p.postSubscription(runID, chSubscribeRequest{
		System:      "extsim",
		Event:       event,
		Correlation: correlation,
	})
	if status != http.StatusCreated {
		p.t.Fatalf("POST subscription for %s status = %d, want %d; body: %s",
			runID, status, http.StatusCreated, data)
	}
	var resp chSubscribeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		p.t.Fatalf("decode subscription response %q: %v", data, err)
	}
	if resp.SubID == "" {
		p.t.Fatalf("subscription response has no sub_id; body: %s", data)
	}
	return resp
}

// postWebhook sends one signed webhook request. Empty fields omit their
// headers.
func (p *connectorhubPlane) postWebhook(body []byte, deliveryID, eventType, signature string) (int, []byte) {
	p.t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if deliveryID != "" {
		headers["X-Extsim-Delivery"] = deliveryID
	}
	if eventType != "" {
		headers["X-Extsim-Event"] = eventType
	}
	if signature != "" {
		headers["X-Extsim-Signature-256"] = signature
	}
	status, data, err := chHTTPDo(http.MethodPost, "http://"+p.webhookAddr+"/hooks/extsim", body, headers)
	if err != nil {
		p.t.Fatalf("POST webhook: %v", err)
	}
	return status, data
}

// chHTTPDo performs one HTTP request. It does not touch testing.T so it can run
// in a goroutine.
func chHTTPDo(method, url string, body []byte, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{Timeout: chHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// chSignWebhook returns the X-Extsim-Signature-256 value for body.
func chSignWebhook(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// chManualEvent builds a job.completed payload. Only the fields connectorhub
// reads are significant; the rest mirrors the extsim callback shape.
func chManualEvent(jobID, project string, at time.Time) []byte {
	payload := map[string]any{
		"job": map[string]any{
			"id":         jobID,
			"project":    project,
			"status":     "completed",
			"conclusion": "succeeded",
			"reason":     "",
			"created_at": at.Add(-time.Second).Format(time.RFC3339Nano),
			"updated_at": at.Format(time.RFC3339Nano),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return data
}

// connectorhubExtsim is the extsim child process.
type connectorhubExtsim struct {
	t *testing.T

	cmd         *exec.Cmd
	jobsAddr    string
	controlAddr string

	mu      sync.Mutex
	log     bytes.Buffer
	waitErr error
	done    chan struct{}
}

// chStartExtsim starts extsim serve and waits for both listening lines.
func chStartExtsim(t *testing.T, bin, callbackURL, keyFile string) *connectorhubExtsim {
	t.Helper()

	x := &connectorhubExtsim{t: t, done: make(chan struct{})}
	cmd := exec.Command(bin, "serve",
		"--listen", "127.0.0.1:0",
		"--control-listen", "127.0.0.1:0",
		"--callback-url", callbackURL,
		"--signing-key-file", keyFile,
		"--job-duration", "1s",
	)
	cmd.Env = os.Environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("extsim stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("extsim stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start extsim: %v", err)
	}
	x.cmd = cmd

	jobsCh := make(chan string, 1)
	controlCh := make(chan string, 1)
	go x.scanOutput(stdout, jobsCh, controlCh)
	go x.drain(stderr)
	go func() {
		x.waitErr = cmd.Wait()
		close(x.done)
	}()

	t.Cleanup(func() { x.stop() })

	deadline := time.After(chExtsimWaitTimeout)
	for x.jobsAddr == "" || x.controlAddr == "" {
		select {
		case addr := <-jobsCh:
			x.jobsAddr = addr
		case addr := <-controlCh:
			x.controlAddr = addr
		case <-x.done:
			t.Fatalf("extsim exited before listening: %v\n%s", x.waitErr, x.output())
		case <-deadline:
			x.stop()
			t.Fatalf("extsim did not report both listening addresses\n%s", x.output())
		}
	}
	return x
}

// scanOutput reads stdout, records it and reports the two listening addresses.
func (x *connectorhubExtsim) scanOutput(r io.Reader, jobsCh, controlCh chan<- string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		x.record(line)
		if addr, ok := chParseListening(line, "jobs"); ok {
			select {
			case jobsCh <- addr:
			default:
			}
		}
		if addr, ok := chParseListening(line, "control"); ok {
			select {
			case controlCh <- addr:
			default:
			}
		}
	}
}

func (x *connectorhubExtsim) drain(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		x.record(scanner.Text())
	}
}

func (x *connectorhubExtsim) record(line string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.log.WriteString(line)
	x.log.WriteByte('\n')
}

func (x *connectorhubExtsim) output() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.log.String()
}

// stop sends SIGTERM and waits for the process, killing it if it does not exit.
func (x *connectorhubExtsim) stop() {
	if x.cmd == nil || x.cmd.Process == nil {
		return
	}
	select {
	case <-x.done:
		return
	default:
	}
	_ = x.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-x.done:
	case <-time.After(5 * time.Second):
		_ = x.cmd.Process.Kill()
		<-x.done
	}
}

// chParseListening extracts the addr attribute of a slog "listening" line for
// the given listener.
func chParseListening(line, want string) (string, bool) {
	fields := strings.Fields(line)
	var msg, listener, addr string
	for _, field := range fields {
		switch {
		case strings.HasPrefix(field, "msg="):
			msg = strings.TrimPrefix(field, "msg=")
		case strings.HasPrefix(field, "listener="):
			listener = strings.TrimPrefix(field, "listener=")
		case strings.HasPrefix(field, "addr="):
			addr = strings.TrimPrefix(field, "addr=")
		}
	}
	if msg != "listening" || listener != want || addr == "" {
		return "", false
	}
	return addr, true
}

// chExtsimJob is the extsim job JSON.
type chExtsimJob struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Reason     string `json:"reason"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// chExtsimDelivery is one callback attempt reported by the control interface.
type chExtsimDelivery struct {
	DeliveryID string `json:"delivery_id"`
	Attempt    int    `json:"attempt"`
	SentAt     string `json:"sent_at"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
}

// plan queues one job plan. Every field may be empty.
func (x *connectorhubExtsim) plan(plan map[string]any) {
	x.t.Helper()
	body, err := json.Marshal(plan)
	if err != nil {
		x.t.Fatalf("marshal extsim plan: %v", err)
	}
	status, data, err := chHTTPDo(http.MethodPost, "http://"+x.controlAddr+"/control/plans", body,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		x.t.Fatalf("POST /control/plans: %v", err)
	}
	if status != http.StatusCreated {
		x.t.Fatalf("POST /control/plans status = %d, want %d; body: %s", status, http.StatusCreated, data)
	}
}

// submitJob creates a job and returns its id.
func (x *connectorhubExtsim) submitJob(project, content string) string {
	x.t.Helper()
	body, err := json.Marshal(map[string]string{"project": project, "content": content})
	if err != nil {
		x.t.Fatalf("marshal extsim job: %v", err)
	}
	status, data, err := chHTTPDo(http.MethodPost, "http://"+x.jobsAddr+"/v1/jobs", body,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		x.t.Fatalf("POST extsim /v1/jobs: %v", err)
	}
	if status != http.StatusCreated {
		x.t.Fatalf("POST extsim /v1/jobs status = %d, want %d; body: %s", status, http.StatusCreated, data)
	}
	var job chExtsimJob
	if err := json.Unmarshal(data, &job); err != nil || job.ID == "" {
		x.t.Fatalf("decode extsim job %q: %v", data, err)
	}
	return job.ID
}

// job fetches one job.
func (x *connectorhubExtsim) job(id string) chExtsimJob {
	x.t.Helper()
	status, data, err := chHTTPDo(http.MethodGet, "http://"+x.jobsAddr+"/v1/jobs/"+id, nil, nil)
	if err != nil {
		x.t.Fatalf("GET extsim job %s: %v", id, err)
	}
	if status != http.StatusOK {
		x.t.Fatalf("GET extsim job %s status = %d, want %d; body: %s", id, status, http.StatusOK, data)
	}
	var job chExtsimJob
	if err := json.Unmarshal(data, &job); err != nil {
		x.t.Fatalf("decode extsim job %q: %v", data, err)
	}
	return job
}

// deliveries fetches the callback attempts of one job.
func (x *connectorhubExtsim) deliveries(jobID string) []chExtsimDelivery {
	x.t.Helper()
	status, data, err := chHTTPDo(http.MethodGet,
		"http://"+x.controlAddr+"/control/jobs/"+jobID+"/deliveries", nil, nil)
	if err != nil {
		x.t.Fatalf("GET extsim deliveries for %s: %v", jobID, err)
	}
	if status != http.StatusOK {
		x.t.Fatalf("GET extsim deliveries for %s status = %d, want %d; body: %s",
			jobID, status, http.StatusOK, data)
	}
	var response struct {
		Deliveries []chExtsimDelivery `json:"deliveries"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		x.t.Fatalf("decode extsim deliveries %q: %v", data, err)
	}
	return response.Deliveries
}

// waitDelivery polls until pred matches one delivery record and returns it.
func (x *connectorhubExtsim) waitDelivery(jobID string, pred func(chExtsimDelivery) bool) chExtsimDelivery {
	x.t.Helper()
	var found chExtsimDelivery
	chWaitFor(x.t, "a matching extsim delivery for "+jobID, func() bool {
		for _, d := range x.deliveries(jobID) {
			if pred(d) {
				found = d
				return true
			}
		}
		return false
	})
	return found
}

// waitJobStatus polls until the job reaches status.
func (x *connectorhubExtsim) waitJobStatus(jobID, status string) chExtsimJob {
	x.t.Helper()
	var job chExtsimJob
	chWaitFor(x.t, "job "+jobID+" status "+status, func() bool {
		job = x.job(jobID)
		return job.Status == status
	})
	return job
}

// callbackBody reconstructs the callback body extsim posts for a job from the
// job's HTTP representation, which uses the same encoding.
func (x *connectorhubExtsim) callbackBody(jobID string) []byte {
	x.t.Helper()
	status, data, err := chHTTPDo(http.MethodGet, "http://"+x.jobsAddr+"/v1/jobs/"+jobID, nil, nil)
	if err != nil {
		x.t.Fatalf("GET extsim job %s: %v", jobID, err)
	}
	if status != http.StatusOK {
		x.t.Fatalf("GET extsim job %s status = %d, want %d; body: %s", jobID, status, http.StatusOK, data)
	}
	trimmed := bytes.TrimSuffix(data, []byte("\n"))
	body := make([]byte, 0, len(trimmed)+8)
	body = append(body, []byte(`{"job":`)...)
	body = append(body, trimmed...)
	body = append(body, '}')
	return body
}

// chEventRecord is one connector_event row.
type chEventRecord struct {
	AppendSeq   int64
	System      string
	DeliveryID  string
	EventType   string
	ProjectIDs  []string
	Correlation map[string]string
	OccurredAt  time.Time
	Payload     []byte
}

// chInboxRecord is one inbox row.
type chInboxRecord struct {
	Seq      int64
	Kind     string
	EventID  string
	DedupKey string
	Payload  []byte
}

// chCompletionRecord is one completion_delivery row.
type chCompletionRecord struct {
	DedupKey  string
	Status    string
	Attempts  int
	LastError string
	Event     []byte
}

// chCompletionPayload is the JSON runcontroller stores in inbox.payload.
type chCompletionPayload struct {
	SubID              string
	System             string
	Event              string
	SubscriptionStatus string
	Terminal           string
	Correlation        map[string]string
	OccurredAt         time.Time
	Source             string
	Detail             string
	DetailTruncated    bool
}

// events returns every connector_event row in append_seq order.
func (e *connectorhubEnv) events() []chEventRecord {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload
		   FROM connector_event ORDER BY append_seq`)
	if err != nil {
		e.t.Fatalf("query connector_event: %v", err)
	}
	defer rows.Close()

	var out []chEventRecord
	for rows.Next() {
		var row chEventRecord
		if err := rows.Scan(&row.AppendSeq, &row.System, &row.DeliveryID, &row.EventType,
			&row.ProjectIDs, &row.Correlation, &row.OccurredAt, &row.Payload); err != nil {
			e.t.Fatalf("scan connector_event: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read connector_event: %v", err)
	}
	return out
}

// eventsForJob filters the event log by correlation job.id.
func chEventsForJob(events []chEventRecord, jobID string) []chEventRecord {
	var out []chEventRecord
	for _, ev := range events {
		if ev.Correlation["job.id"] == jobID {
			out = append(out, ev)
		}
	}
	return out
}

// eventsForDelivery filters the event log by delivery id.
func chEventsForDelivery(events []chEventRecord, deliveryID string) []chEventRecord {
	var out []chEventRecord
	for _, ev := range events {
		if ev.DeliveryID == deliveryID {
			out = append(out, ev)
		}
	}
	return out
}

// inbox returns the inbox rows of a run in sequence order.
func (e *connectorhubEnv) inbox(runID string) []chInboxRecord {
	e.t.Helper()
	// Every run starts with its prompt as a human_message row. These helpers
	// only look at subscription completion events.
	rows, err := e.pool.Query(context.Background(),
		`SELECT inbox_seq, kind, event_id, dedup_key, payload
		   FROM inbox
		  WHERE run_id = $1 AND kind = 'subscription_completed'
		  ORDER BY inbox_seq`, runID)
	if err != nil {
		e.t.Fatalf("query inbox: %v", err)
	}
	defer rows.Close()

	var out []chInboxRecord
	for rows.Next() {
		var row chInboxRecord
		if err := rows.Scan(&row.Seq, &row.Kind, &row.EventID, &row.DedupKey, &row.Payload); err != nil {
			e.t.Fatalf("scan inbox: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read inbox: %v", err)
	}
	return out
}

// completions returns the completion_delivery rows of a subscription.
func (e *connectorhubEnv) completions(subID string) []chCompletionRecord {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT dedup_key, status, attempts, last_error, event
		   FROM completion_delivery WHERE sub_id = $1 ORDER BY dedup_key`, subID)
	if err != nil {
		e.t.Fatalf("query completion_delivery: %v", err)
	}
	defer rows.Close()

	var out []chCompletionRecord
	for rows.Next() {
		var row chCompletionRecord
		if err := rows.Scan(&row.DedupKey, &row.Status, &row.Attempts, &row.LastError, &row.Event); err != nil {
			e.t.Fatalf("scan completion_delivery: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read completion_delivery: %v", err)
	}
	return out
}

// subscriptionStatus returns the status and terminal of a subscription.
func (e *connectorhubEnv) subscriptionStatus(subID string) (status, terminal string) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, terminal FROM subscription WHERE sub_id = $1`, subID,
	).Scan(&status, &terminal); err != nil {
		e.t.Fatalf("query subscription %s: %v", subID, err)
	}
	return status, terminal
}

// exec runs one test-only SQL statement.
func (e *connectorhubEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), query, args...); err != nil {
		e.t.Fatalf("execute test SQL %q: %v", query, err)
	}
}

// installOneShotClaimError makes the first subscription_claim insert fail. A
// sequence records the attempt because a raised exception rolls the
// transaction back.
func (e *connectorhubEnv) installOneShotClaimError() {
	e.exec(`CREATE SEQUENCE ch_test_claim_error_seq`)
	e.exec(`CREATE FUNCTION ch_test_claim_error() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('ch_test_claim_error_seq') <= 1 THEN
				RAISE EXCEPTION 'injected subscription_claim insert failure';
			END IF;
			RETURN NEW;
		END;
		$$`)
	e.exec(`CREATE TRIGGER ch_test_claim_error_trg BEFORE INSERT ON subscription_claim
		FOR EACH ROW EXECUTE FUNCTION ch_test_claim_error()`)
}

// installOneShotInboxError makes the first inbox insert fail, for the
// next-scan retry test.
func (e *connectorhubEnv) installOneShotInboxError() {
	e.exec(`CREATE SEQUENCE ch_test_inbox_error_seq`)
	e.exec(`CREATE FUNCTION ch_test_inbox_error() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('ch_test_inbox_error_seq') <= 1 THEN
				RAISE EXCEPTION 'injected inbox insert failure';
			END IF;
			RETURN NEW;
		END;
		$$`)
	e.exec(`CREATE TRIGGER ch_test_inbox_error_trg BEFORE INSERT ON inbox
		FOR EACH ROW EXECUTE FUNCTION ch_test_inbox_error()`)
}

// installPersistentInboxError makes every inbox insert fail until
// disableInboxError is called, for the restart test. The flag is a table
// because a raised exception rolls back anything the trigger itself writes.
func (e *connectorhubEnv) installPersistentInboxError() {
	e.exec(`CREATE TABLE ch_test_inbox_error_flag (enabled boolean NOT NULL)`)
	e.exec(`INSERT INTO ch_test_inbox_error_flag (enabled) VALUES (true)`)
	e.exec(`CREATE FUNCTION ch_test_inbox_error_all() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT enabled FROM ch_test_inbox_error_flag) THEN
				RAISE EXCEPTION 'injected inbox insert failure';
			END IF;
			RETURN NEW;
		END;
		$$`)
	e.exec(`CREATE TRIGGER ch_test_inbox_error_all_trg BEFORE INSERT ON inbox
		FOR EACH ROW EXECUTE FUNCTION ch_test_inbox_error_all()`)
}

// disableInboxError turns the persistent injection off.
func (e *connectorhubEnv) disableInboxError() {
	e.exec(`UPDATE ch_test_inbox_error_flag SET enabled = false`)
}

// installSlowSubscription widens the window between the registration snapshot
// and the rescan by sleeping inside the subscription insert.
func (e *connectorhubEnv) installSlowSubscription() {
	e.exec(`CREATE TABLE ch_test_slow_subscription (enabled boolean NOT NULL)`)
	e.exec(`INSERT INTO ch_test_slow_subscription (enabled) VALUES (true)`)
	e.exec(`CREATE FUNCTION ch_test_slow_subscription_fn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT enabled FROM ch_test_slow_subscription) THEN
				PERFORM pg_sleep(2.5);
			END IF;
			RETURN NEW;
		END;
		$$`)
	e.exec(`CREATE TRIGGER ch_test_slow_subscription_trg AFTER INSERT ON subscription
		FOR EACH ROW EXECUTE FUNCTION ch_test_slow_subscription_fn()`)
}

// disableSlowSubscription stops the sleep for later registrations.
func (e *connectorhubEnv) disableSlowSubscription() {
	e.exec(`UPDATE ch_test_slow_subscription SET enabled = false`)
}

// decodeCompletion decodes the inbox payload.
func chDecodeCompletion(t *testing.T, row chInboxRecord) chCompletionPayload {
	t.Helper()
	if len(row.Payload) == 0 {
		t.Fatalf("inbox row %d has no payload", row.Seq)
	}
	var payload chCompletionPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode inbox payload %q: %v", row.Payload, err)
	}
	return payload
}

// chWaitFor polls cond until it is true or the timeout expires.
func chWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(chWaitTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", chWaitTimeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// chRequireInboxCount waits for the run to have exactly n inbox rows.
func chRequireInboxCount(t *testing.T, env *connectorhubEnv, runID string, n int) []chInboxRecord {
	t.Helper()
	var rows []chInboxRecord
	chWaitFor(t, fmt.Sprintf("%d inbox rows for run %s", n, runID), func() bool {
		rows = env.inbox(runID)
		return len(rows) == n
	})
	return rows
}

// testConnectorhubCallbackBeforeRegistration covers the first test bullet: the
// callback arrives before the subscription is registered, so registration
// backtracks and completes it at once.
func testConnectorhubCallbackBeforeRegistration(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	env.extsim.plan(map[string]any{"duration_ms": 100})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)

	delivery := env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
		return d.StatusCode == http.StatusOK
	})
	events := chEventsForDelivery(env.events(), delivery.DeliveryID)
	if len(events) != 1 {
		t.Fatalf("connector_event rows for delivery %s = %d, want 1", delivery.DeliveryID, len(events))
	}
	event := events[0]

	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "completed" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "completed")
	}
	if event.AppendSeq >= sub.SinceCursor {
		t.Errorf("event append_seq = %d, want it below since_cursor %d", event.AppendSeq, sub.SinceCursor)
	}

	rows := chRequireInboxCount(t, env, runID, 1)
	payload := chDecodeCompletion(t, rows[0])
	if rows[0].Kind != "subscription_completed" {
		t.Errorf("inbox kind = %q, want %q", rows[0].Kind, "subscription_completed")
	}
	if payload.SubID != sub.SubID {
		t.Errorf("payload sub_id = %q, want %q", payload.SubID, sub.SubID)
	}
	if payload.Source != "webhook" {
		t.Errorf("payload source = %q, want %q", payload.Source, "webhook")
	}
	if payload.Terminal != "succeeded" {
		t.Errorf("payload terminal = %q, want %q", payload.Terminal, "succeeded")
	}
	if payload.SubscriptionStatus != "completed" || payload.Event != "job.completed" || payload.System != "extsim" {
		t.Errorf("payload = %+v, want completed job.completed from extsim", payload)
	}
	if payload.Correlation["job.id"] != jobID {
		t.Errorf("payload correlation job.id = %q, want %q", payload.Correlation["job.id"], jobID)
	}
	job := env.extsim.job(jobID)
	updatedAt, err := time.Parse(time.RFC3339Nano, job.UpdatedAt)
	if err != nil {
		t.Fatalf("parse job updated_at %q: %v", job.UpdatedAt, err)
	}
	if !payload.OccurredAt.Equal(updatedAt) {
		t.Errorf("payload occurred_at = %s, want the event time %s", payload.OccurredAt, updatedAt)
	}
	if !strings.Contains(payload.Detail, `"job"`) {
		t.Errorf("payload detail does not contain the raw job payload: %q", payload.Detail)
	}

	deliveries := env.completions(sub.SubID)
	if len(deliveries) != 1 || deliveries[0].Status != "delivered" {
		t.Fatalf("completion deliveries = %+v, want one delivered record", deliveries)
	}
	_, terminal := env.subscriptionStatus(sub.SubID)
	if terminal != "succeeded" {
		t.Errorf("stored terminal = %q, want %q", terminal, "succeeded")
	}
}

// testConnectorhubDroppedCallbackVerified covers the second bullet: the
// callback is dropped, and the status query at registration produces a
// verify-sourced completion.
func testConnectorhubDroppedCallbackVerified(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	env.extsim.plan(map[string]any{"duration_ms": 100, "callback": "drop"})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
	job := env.extsim.waitJobStatus(jobID, "completed")
	if job.Conclusion != "succeeded" {
		t.Fatalf("job conclusion = %q, want %q", job.Conclusion, "succeeded")
	}

	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "completed" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "completed")
	}

	rows := chRequireInboxCount(t, env, runID, 1)
	payload := chDecodeCompletion(t, rows[0])
	if payload.Source != "verify" {
		t.Errorf("payload source = %q, want %q", payload.Source, "verify")
	}
	if payload.Terminal != "succeeded" {
		t.Errorf("payload terminal = %q, want %q", payload.Terminal, "succeeded")
	}
	if !strings.Contains(payload.Detail, `"status":"completed"`) ||
		!strings.Contains(payload.Detail, jobID) {
		t.Errorf("payload detail is not the job status response: %q", payload.Detail)
	}
	if payload.OccurredAt.IsZero() {
		t.Error("payload occurred_at is zero, want the query time")
	}
	if events := env.events(); len(events) != 0 {
		t.Errorf("connector_event rows = %d, want 0 for a dropped callback", len(events))
	}

	deliveries := env.completions(sub.SubID)
	if len(deliveries) != 1 || deliveries[0].Status != "delivered" {
		t.Fatalf("completion deliveries = %+v, want one delivered record", deliveries)
	}
}

// testConnectorhubConcurrentRegistration covers the third bullet: events that
// commit before the snapshot, between the snapshot and the rescan, and after
// the rescan all complete the subscription exactly once.
func testConnectorhubConcurrentRegistration(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	assertCompletedOnce := func(t *testing.T, runID string, sub chSubscribeResponse, jobID string) {
		t.Helper()
		chWaitFor(t, "the subscription to be completed", func() bool {
			status, _ := env.subscriptionStatus(sub.SubID)
			return status == "completed"
		})
		rows := chRequireInboxCount(t, env, runID, 1)
		if chDecodeCompletion(t, rows[0]).SubID != sub.SubID {
			t.Errorf("inbox payload belongs to another subscription")
		}
		deliveries := env.completions(sub.SubID)
		if len(deliveries) != 1 || deliveries[0].Status != "delivered" {
			t.Fatalf("completion deliveries = %+v, want one delivered record", deliveries)
		}
		// 三种时序都应由回调事件完成；状态查询只作为回调丢失时的兜底。
		var completion struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(deliveries[0].Event, &completion); err != nil {
			t.Fatalf("decode completion event: %v", err)
		}
		if completion.Source != "webhook" {
			t.Errorf("completion source = %q, want webhook", completion.Source)
		}
		if events := chEventsForJob(env.events(), jobID); len(events) != 1 {
			t.Errorf("connector_event rows for job %s = %d, want 1", jobID, len(events))
		}
	}

	t.Run("before snapshot", func(t *testing.T) {
		runID := plane.createRun()
		env.extsim.plan(map[string]any{"duration_ms": 100})
		jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
		env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
			return d.StatusCode == http.StatusOK
		})
		sub := plane.subscribeOK(runID, "job.completed", map[string]string{
			"job.project": "demo",
			"job.id":      jobID,
		})
		assertCompletedOnce(t, runID, sub, jobID)
	})

	t.Run("between snapshot and rescan", func(t *testing.T) {
		runID := plane.createRun()
		env.installSlowSubscription()
		defer env.disableSlowSubscription()

		// The callback is delayed by one second; the registration transaction
		// sleeps for 2.5 seconds after taking its snapshot, so the event
		// commits inside that window.
		env.extsim.plan(map[string]any{"duration_ms": 100, "callback_delay_ms": 1000})
		jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)

		request, err := json.Marshal(chSubscribeRequest{
			System:      "extsim",
			Event:       "job.completed",
			Correlation: map[string]string{"job.project": "demo", "job.id": jobID},
		})
		if err != nil {
			t.Fatalf("marshal subscription request: %v", err)
		}
		type result struct {
			status int
			body   []byte
			err    error
		}
		done := make(chan result, 1)
		go func() {
			status, body, err := chHTTPDo(http.MethodPost,
				"http://"+plane.connectorAddr+"/v1/runs/"+runID+"/subscriptions", request,
				map[string]string{"Content-Type": "application/json"})
			done <- result{status: status, body: body, err: err}
		}()

		var event chEventRecord
		chWaitFor(t, "the delayed callback to be recorded", func() bool {
			events := chEventsForJob(env.events(), jobID)
			if len(events) == 0 {
				return false
			}
			event = events[0]
			return true
		})

		var res result
		select {
		case res = <-done:
		case <-time.After(chWaitTimeout):
			t.Fatal("subscription registration did not return")
		}
		if res.err != nil {
			t.Fatalf("POST subscription: %v", res.err)
		}
		if res.status != http.StatusCreated {
			t.Fatalf("POST subscription status = %d, want %d; body: %s", res.status, http.StatusCreated, res.body)
		}
		var sub chSubscribeResponse
		if err := json.Unmarshal(res.body, &sub); err != nil {
			t.Fatalf("decode subscription response %q: %v", res.body, err)
		}
		if sub.SinceCursor > event.AppendSeq {
			t.Errorf("event append_seq = %d, want it at or after since_cursor %d", event.AppendSeq, sub.SinceCursor)
		}
		assertCompletedOnce(t, runID, sub, jobID)
	})

	t.Run("after rescan", func(t *testing.T) {
		runID := plane.createRun()
		env.extsim.plan(map[string]any{"duration_ms": 1500})
		jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
		sub := plane.subscribeOK(runID, "job.completed", map[string]string{
			"job.project": "demo",
			"job.id":      jobID,
		})
		if sub.Status != "active" {
			t.Fatalf("subscription status = %q, want %q before the callback", sub.Status, "active")
		}
		env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
			return d.StatusCode == http.StatusOK
		})
		assertCompletedOnce(t, runID, sub, jobID)
	})
}

// testConnectorhubDuplicateDelivery covers the fourth bullet: a resent
// delivery id and a duplicate callback with a new delivery id both leave a
// single inbox row.
func testConnectorhubDuplicateDelivery(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	env.extsim.plan(map[string]any{"duration_ms": 100, "callback": "duplicate"})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)

	var first chExtsimDelivery
	chWaitFor(t, "two successful callback deliveries", func() bool {
		seen := make(map[string]bool)
		for _, d := range env.extsim.deliveries(jobID) {
			if d.StatusCode == http.StatusOK {
				seen[d.DeliveryID] = true
				if first.DeliveryID == "" {
					first = d
				}
			}
		}
		return len(seen) >= 2
	})
	if events := chEventsForJob(env.events(), jobID); len(events) != 2 {
		t.Fatalf("connector_event rows for job %s = %d, want 2", jobID, len(events))
	}

	// Resend the original delivery under the same delivery id. The body comes
	// from the job's HTTP representation, which is what extsim posted.
	body := env.extsim.callbackBody(jobID)
	status, data := plane.postWebhook(body, first.DeliveryID, "job.completed", chSignWebhook(env.key, body))
	if status != http.StatusOK {
		t.Fatalf("resent webhook status = %d, want %d; body: %s", status, http.StatusOK, data)
	}
	if events := chEventsForJob(env.events(), jobID); len(events) != 2 {
		t.Errorf("connector_event rows after the resend = %d, want 2", len(events))
	}

	runID := plane.createRun()
	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "completed" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "completed")
	}
	rows := chRequireInboxCount(t, env, runID, 1)
	if payload := chDecodeCompletion(t, rows[0]); payload.Source != "webhook" {
		t.Errorf("payload source = %q, want %q", payload.Source, "webhook")
	}
	if deliveries := env.completions(sub.SubID); len(deliveries) != 1 || deliveries[0].Status != "delivered" {
		t.Fatalf("completion deliveries = %+v, want one delivered record", deliveries)
	}
}

// testConnectorhubMatchDatabaseError covers the fifth bullet: the first match
// fails with a database error and returns 500; extsim retries the same
// delivery and the subscription completes.
func testConnectorhubMatchDatabaseError(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	env.extsim.plan(map[string]any{"duration_ms": 2500})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "active" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "active")
	}

	env.installOneShotClaimError()

	failed := env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
		return d.StatusCode == http.StatusInternalServerError
	})
	if failed.Error != "" {
		t.Logf("extsim recorded the 500 response as %q", failed.Error)
	}

	chWaitFor(t, "the subscription to complete after the retry", func() bool {
		status, _ := env.subscriptionStatus(sub.SubID)
		return status == "completed"
	})
	rows := chRequireInboxCount(t, env, runID, 1)
	if payload := chDecodeCompletion(t, rows[0]); payload.Source != "webhook" {
		t.Errorf("payload source = %q, want %q", payload.Source, "webhook")
	}
	if events := chEventsForJob(env.events(), jobID); len(events) != 1 {
		t.Errorf("connector_event rows for job %s = %d, want 1", jobID, len(events))
	}
	deliveries := env.completions(sub.SubID)
	if len(deliveries) != 1 || deliveries[0].Status != "delivered" {
		t.Fatalf("completion deliveries = %+v, want one delivered record", deliveries)
	}
}

// testConnectorhubAppendInboxRetry covers the sixth bullet: AppendInbox fails
// once, the record stays pending, and the next scan delivers it.
func testConnectorhubAppendInboxRetry(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.id": "job-append-retry",
	})
	if sub.Status != "active" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "active")
	}

	env.installOneShotInboxError()

	body := chManualEvent("job-append-retry", "demo", time.Now().UTC())
	status, data := plane.postWebhook(body, "delivery-append-retry", "job.completed",
		chSignWebhook(env.key, body))
	if status != http.StatusOK {
		t.Fatalf("webhook status = %d, want %d; body: %s", status, http.StatusOK, data)
	}

	var pending chCompletionRecord
	chWaitFor(t, "the completion record to record a pending failure", func() bool {
		rows := env.completions(sub.SubID)
		if len(rows) != 1 {
			return false
		}
		pending = rows[0]
		return pending.Status == "pending" && pending.Attempts >= 1 && pending.LastError != ""
	})

	chWaitFor(t, "the next scan to deliver the pending record", func() bool {
		rows := env.completions(sub.SubID)
		return len(rows) == 1 && rows[0].Status == "delivered"
	})
	delivered := env.completions(sub.SubID)[0]
	if delivered.Attempts < 2 {
		t.Errorf("delivery attempts = %d, want at least 2 after one failure and one retry", delivered.Attempts)
	}
	rows := chRequireInboxCount(t, env, runID, 1)
	if payload := chDecodeCompletion(t, rows[0]); payload.Source != "webhook" {
		t.Errorf("payload source = %q, want %q", payload.Source, "webhook")
	}
}

// testConnectorhubDeliveryAfterRestart covers the restart half of the sixth
// bullet: a pending record left by the stopped control plane is delivered by
// the startup scan of the new one. Both planes use the same webhook address.
func testConnectorhubDeliveryAfterRestart(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	first := env.startPlane(t, env.webhookAddr)

	runID := first.createRun()
	env.extsim.plan(map[string]any{"duration_ms": 100})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
	env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
		return d.StatusCode == http.StatusOK
	})

	env.installPersistentInboxError()
	sub := first.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "completed" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "completed")
	}

	var pending chCompletionRecord
	chWaitFor(t, "the completion record to stay pending", func() bool {
		rows := env.completions(sub.SubID)
		if len(rows) != 1 {
			return false
		}
		pending = rows[0]
		return pending.Status == "pending" && pending.Attempts >= 1 && pending.LastError != ""
	})
	if rows := env.inbox(runID); len(rows) != 0 {
		t.Fatalf("inbox rows before the restart = %d, want 0", len(rows))
	}

	// Stop the first control plane and lift the failure injection. The pending
	// record stays in the database.
	if err := first.stop(); err != nil {
		t.Fatalf("first control plane shutdown returned %v, want nil", err)
	}
	env.disableInboxError()

	// The second plane reuses the same webhook address; its startup scan
	// delivers the pending record.
	env.startPlane(t, env.webhookAddr)

	rows := chRequireInboxCount(t, env, runID, 1)
	payload := chDecodeCompletion(t, rows[0])
	if payload.SubID != sub.SubID || payload.Source != "webhook" {
		t.Errorf("delivered payload = %+v, want the webhook completion of %s", payload, sub.SubID)
	}
	// 收件箱行与投递状态分两个事务提交，因此等待最终状态，而不是读一次。
	chWaitFor(t, "the pending completion to be marked delivered", func() bool {
		deliveries := env.completions(sub.SubID)
		return len(deliveries) == 1 && deliveries[0].Status == "delivered" && deliveries[0].Attempts >= 2
	})
	if events := chEventsForJob(env.events(), jobID); len(events) != 1 {
		t.Errorf("connector_event rows for job %s = %d, want 1", jobID, len(events))
	}
}

// testConnectorhubCancelAfterRunEnd covers the seventh bullet: the scan
// cancels the active subscription after the run ends, and a later matching
// event writes nothing to the inbox.
func testConnectorhubCancelAfterRunEnd(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	env.extsim.plan(map[string]any{"duration_ms": 15000})
	jobID := env.extsim.submitJob("demo", `{"owner":"alice"}`)
	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.project": "demo",
		"job.id":      jobID,
	})
	if sub.Status != "active" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "active")
	}

	plane.endRun(runID)
	chWaitFor(t, "the scan to cancel the subscription of the ended run", func() bool {
		status, _ := env.subscriptionStatus(sub.SubID)
		return status == "cancelled"
	})

	// The job completes after the cancellation; the callback must not produce
	// a completion record.
	env.extsim.waitJobStatus(jobID, "completed")
	env.extsim.waitDelivery(jobID, func(d chExtsimDelivery) bool {
		return d.StatusCode == http.StatusOK
	})
	if rows := env.inbox(runID); len(rows) != 0 {
		t.Errorf("inbox rows after the run ended = %d, want 0", len(rows))
	}
	if deliveries := env.completions(sub.SubID); len(deliveries) != 0 {
		t.Errorf("completion deliveries after the run ended = %+v, want none", deliveries)
	}
	if events := chEventsForJob(env.events(), jobID); len(events) != 1 {
		t.Errorf("connector_event rows for job %s = %d, want 1", jobID, len(events))
	}
}

// testConnectorhubSubscriptionValidation covers the eighth bullet: the 400
// cases name the unmet condition, an unknown run is 404, and an ended run is
// 409.
func testConnectorhubSubscriptionValidation(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()

	cases := []struct {
		name        string
		request     chSubscribeRequest
		wantMessage string
	}{
		{
			name:        "system is required",
			request:     chSubscribeRequest{Event: "job.completed", Correlation: map[string]string{"job.id": "job-x"}},
			wantMessage: "system is required",
		},
		{
			name:        "unknown system",
			request:     chSubscribeRequest{System: "unknown", Event: "job.completed", Correlation: map[string]string{"job.id": "job-x"}},
			wantMessage: "not available",
		},
		{
			name:        "event is required",
			request:     chSubscribeRequest{System: "extsim", Correlation: map[string]string{"job.id": "job-x"}},
			wantMessage: "event is required",
		},
		{
			name:        "event outside the catalogue",
			request:     chSubscribeRequest{System: "extsim", Event: "job.other", Correlation: map[string]string{"job.id": "job-x"}},
			wantMessage: "not in the event catalogue",
		},
		{
			name:        "correlation key not accepted",
			request:     chSubscribeRequest{System: "extsim", Event: "job.completed", Correlation: map[string]string{"job.id": "job-x", "job.bogus": "y"}},
			wantMessage: "not accepted",
		},
		{
			name:        "missing identity key",
			request:     chSubscribeRequest{System: "extsim", Event: "job.completed", Correlation: map[string]string{"job.project": "demo"}},
			wantMessage: "identity key",
		},
		{
			name:        "project binding not bound",
			request:     chSubscribeRequest{System: "extsim", Event: "job.completed", Correlation: map[string]string{"job.id": "job-x", "job.project": "other"}},
			wantMessage: "not bound to the project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, data := plane.postSubscription(runID, tc.request)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", status, http.StatusBadRequest, data)
			}
			var response struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatalf("decode error body %q: %v", data, err)
			}
			if !strings.Contains(response.Error, tc.wantMessage) {
				t.Errorf("error = %q, want it to mention %q", response.Error, tc.wantMessage)
			}
		})
	}

	t.Run("unknown run", func(t *testing.T) {
		status, data := plane.postSubscription("zzzzzzzzzzzz", chSubscribeRequest{
			System: "extsim", Event: "job.completed",
			Correlation: map[string]string{"job.id": "job-x"},
		})
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body: %s", status, http.StatusNotFound, data)
		}
		if !strings.Contains(string(data), "run not found") {
			t.Errorf("body = %s, want it to mention run not found", data)
		}
	})

	t.Run("ended run", func(t *testing.T) {
		plane.endRun(runID)
		status, data := plane.postSubscription(runID, chSubscribeRequest{
			System: "extsim", Event: "job.completed",
			Correlation: map[string]string{"job.id": "job-x"},
		})
		if status != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body: %s", status, http.StatusConflict, data)
		}
		if !strings.Contains(string(data), "ended or failed") {
			t.Errorf("body = %s, want it to mention that the run ended or failed", data)
		}
	})
}

// testConnectorhubWebhookRejection covers the ninth bullet: a bad signature is
// 401 and a missing delivery id is 400, neither writing the event log; an
// unbound project value is written with empty project_ids and matches nothing.
func testConnectorhubWebhookRejection(t *testing.T, bin string) {
	env := newConnectorhubEnv(t, bin)
	plane := env.startPlane(t, env.webhookAddr)

	runID := plane.createRun()
	sub := plane.subscribeOK(runID, "job.completed", map[string]string{
		"job.id": "job-unbound",
	})
	if sub.Status != "active" {
		t.Fatalf("subscription status = %q, want %q", sub.Status, "active")
	}

	body := chManualEvent("job-unbound", "demo", time.Now().UTC())
	before := len(env.events())

	wrongKey := append(append([]byte(nil), env.key...), 'x')
	status, _ := plane.postWebhook(body, "delivery-bad-signature", "job.completed",
		chSignWebhook(wrongKey, body))
	if status != http.StatusUnauthorized {
		t.Errorf("bad signature status = %d, want %d", status, http.StatusUnauthorized)
	}

	status, _ = plane.postWebhook(body, "", "job.completed", chSignWebhook(env.key, body))
	if status != http.StatusBadRequest {
		t.Errorf("missing delivery id status = %d, want %d", status, http.StatusBadRequest)
	}

	if after := len(env.events()); after != before {
		t.Errorf("connector_event rows after rejected webhooks = %d, want %d", after, before)
	}

	unbound := chManualEvent("job-unbound", "not-bound", time.Now().UTC())
	status, data := plane.postWebhook(unbound, "delivery-unbound-project", "job.completed",
		chSignWebhook(env.key, unbound))
	if status != http.StatusOK {
		t.Fatalf("unbound project webhook status = %d, want %d; body: %s", status, http.StatusOK, data)
	}
	events := chEventsForDelivery(env.events(), "delivery-unbound-project")
	if len(events) != 1 {
		t.Fatalf("connector_event rows for the unbound event = %d, want 1", len(events))
	}
	if len(events[0].ProjectIDs) != 0 {
		t.Errorf("project_ids = %v, want empty", events[0].ProjectIDs)
	}
	if status, _ := env.subscriptionStatus(sub.SubID); status != "active" {
		t.Errorf("subscription status = %q, want %q", status, "active")
	}
	if rows := env.inbox(runID); len(rows) != 0 {
		t.Errorf("inbox rows = %d, want 0", len(rows))
	}
	if deliveries := env.completions(sub.SubID); len(deliveries) != 0 {
		t.Errorf("completion deliveries = %+v, want none", deliveries)
	}
}
