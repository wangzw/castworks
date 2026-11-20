package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for concurrent use by the child process
// copy goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logCollector drains the serve process's standard output.
type logCollector struct {
	mu    sync.Mutex
	lines []string
	done  chan struct{}
}

func newLogCollector(r io.Reader) *logCollector {
	c := &logCollector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			c.mu.Lock()
			c.lines = append(c.lines, scanner.Text())
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *logCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// logAttrs splits a slog text line into its key=value attributes.
func logAttrs(line string) map[string]string {
	attrs := make(map[string]string)
	for _, field := range strings.Fields(line) {
		if key, value, ok := strings.Cut(field, "="); ok {
			attrs[key] = value
		}
	}
	return attrs
}

// serveProcess is a running extsim serve subprocess.
type serveProcess struct {
	cmd        *exec.Cmd
	logs       *logCollector
	stderr     *syncBuffer
	jobsURL    string
	controlURL string
	waitDone   chan struct{}
	waitErr    error
}

// startServe starts extsim serve with both listeners on ephemeral loopback
// ports and reads their addresses from the listening log lines.
func startServe(t *testing.T, callbackURL, keyPath string) *serveProcess {
	t.Helper()
	cmd := exec.Command(extsimTestBinary,
		"serve",
		"--listen", "127.0.0.1:0",
		"--control-listen", "127.0.0.1:0",
		"--callback-url", callbackURL,
		"--signing-key-file", keyPath,
		"--job-duration", "50ms",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}

	sp := &serveProcess{
		cmd:      cmd,
		logs:     newLogCollector(stdout),
		stderr:   stderr,
		waitDone: make(chan struct{}),
	}
	go func() {
		sp.waitErr = cmd.Wait()
		close(sp.waitDone)
	}()
	t.Cleanup(func() {
		select {
		case <-sp.waitDone:
		default:
			_ = cmd.Process.Kill()
			<-sp.waitDone
		}
	})

	sp.jobsURL = "http://" + sp.waitListener(t, "jobs")
	sp.controlURL = "http://" + sp.waitListener(t, "control")
	return sp
}

// waitListener returns the address of the named listener from the listening
// log lines.
func (sp *serveProcess) waitListener(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, line := range sp.logs.snapshot() {
			attrs := logAttrs(line)
			if attrs["msg"] == "listening" && attrs["listener"] == name && attrs["addr"] != "" {
				return attrs["addr"]
			}
		}
		select {
		case <-sp.waitDone:
			t.Fatalf("serve exited before the %s listener was logged: %v\nstderr: %s\nstdout:\n%s",
				name, sp.waitErr, sp.stderr.String(), strings.Join(sp.logs.snapshot(), "\n"))
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the %s listening line; stdout:\n%s", name, strings.Join(sp.logs.snapshot(), "\n"))
		}
	}
}

// stop sends SIGTERM and requires a clean exit.
func (sp *serveProcess) stop(t *testing.T) {
	t.Helper()
	if err := sp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case <-sp.waitDone:
		if sp.waitErr != nil {
			t.Fatalf("serve exited with %v after SIGTERM\nstderr: %s", sp.waitErr, sp.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = sp.cmd.Process.Kill()
		<-sp.waitDone
		t.Fatalf("serve did not exit after SIGTERM")
	}
}

// waitForLog waits until a log line contains substr.
func (sp *serveProcess) waitForLog(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range sp.logs.snapshot() {
			if strings.Contains(line, substr) {
				return
			}
		}
		select {
		case <-sp.waitDone:
			t.Fatalf("serve exited before logging %q: %v\nstderr: %s\nstdout:\n%s",
				substr, sp.waitErr, sp.stderr.String(), strings.Join(sp.logs.snapshot(), "\n"))
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log %q; stdout:\n%s", substr, strings.Join(sp.logs.snapshot(), "\n"))
		}
	}
}

// doJSON sends one JSON request and returns its status and body.
func doJSON(t *testing.T, method, target string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, data
}

// signTestBody signs body with key exactly as the callback sender does.
func signTestBody(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type receivedCallback struct {
	method    string
	event     string
	delivery  string
	signature string
	body      []byte
}

func TestServeConfigDefaults(t *testing.T) {
	keyPath := writeTestKey(t, "key\n")
	cfg, err := parseServeConfig([]string{"--signing-key-file", keyPath}, io.Discard)
	if err != nil {
		t.Fatalf("parseServeConfig: %v", err)
	}
	if cfg.Listen != serveDefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Listen, serveDefaultListen)
	}
	if cfg.ControlListen != serveDefaultControlListen {
		t.Errorf("control listen = %q, want %q", cfg.ControlListen, serveDefaultControlListen)
	}
	if cfg.CallbackURL != "http://127.0.0.1:9092"+callbackWebhookPath {
		t.Errorf("callback URL = %q, want the default webhook receiver", cfg.CallbackURL)
	}
	if cfg.JobDuration != serveDefaultJobDuration {
		t.Errorf("job duration = %s, want %s", cfg.JobDuration, serveDefaultJobDuration)
	}
	if cfg.SigningKeyFile != keyPath {
		t.Errorf("signing key file = %q, want %q", cfg.SigningKeyFile, keyPath)
	}
}

func TestServeConfigErrors(t *testing.T) {
	keyPath := writeTestKey(t, "key\n")
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing signing key", wantErr: "signing-key-file"},
		{name: "zero job duration", args: []string{"--signing-key-file", keyPath, "--job-duration", "0s"}, wantErr: "job-duration"},
		{name: "non-loopback control", args: []string{"--signing-key-file", keyPath, "--control-listen", "0.0.0.0:9101"}, wantErr: "loopback"},
		{name: "invalid callback URL", args: []string{"--signing-key-file", keyPath, "--callback-url", "ftp://example.com/hook"}, wantErr: "callback-url"},
		{name: "empty listen", args: []string{"--signing-key-file", keyPath, "--listen", ""}, wantErr: "listen"},
		{name: "unknown flag", args: []string{"--signing-key-file", keyPath, "--bogus"}, wantErr: "bogus"},
		{name: "extra argument", args: []string{"--signing-key-file", keyPath, "extra"}, wantErr: "unexpected arguments"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServeConfig(tc.args, io.Discard)
			if err == nil {
				t.Fatalf("parseServeConfig succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateControlListen(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{addr: "127.0.0.1:9101"},
		{addr: "127.0.0.2:0"},
		{addr: "[::1]:9101"},
		{addr: "localhost:9101"},
		{addr: "0.0.0.0:9101", wantErr: true},
		{addr: "10.1.2.3:9101", wantErr: true},
		{addr: ":9101", wantErr: true},
		{addr: "127.0.0.1", wantErr: true},
		{addr: "example.com:9101", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			err := validateControlListen(tc.addr)
			if tc.wantErr && err == nil {
				t.Errorf("validateControlListen(%q) = nil, want an error", tc.addr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateControlListen(%q) = %v, want nil", tc.addr, err)
			}
		})
	}
}

// TestServeEndToEnd starts serve as a subprocess and runs the whole flow:
// queue a plan, submit a job, let it complete at the planned duration, receive
// the signed callback and read the delivery record.
func TestServeEndToEnd(t *testing.T) {
	const key = "test-signing-key"
	keyPath := writeTestKey(t, key+"\n")

	callbacks := make(chan receivedCallback, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		callbacks <- receivedCallback{
			method:    r.Method,
			event:     r.Header.Get(callbackEventHeader),
			delivery:  r.Header.Get(callbackDeliveryHeader),
			signature: r.Header.Get(callbackSignatureHeader),
			body:      body,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	sp := startServe(t, receiver.URL+callbackWebhookPath, keyPath)

	status, body := doJSON(t, http.MethodPost, sp.controlURL+controlPlansPath, map[string]any{
		"duration_ms": 40,
		"callback":    "deliver",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /control/plans = %d, want 201: %s", status, body)
	}

	content := `{"owner": "acceptance-tester"}`
	status, body = doJSON(t, http.MethodPost, sp.jobsURL+"/v1/jobs", map[string]string{
		"project": "demo",
		"content": content,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, want 201: %s", status, body)
	}
	created := decodeJob(t, body)
	if created.ID == "" {
		t.Fatalf("created job has no id: %s", body)
	}
	if created.Status != JobStatusQueued {
		t.Errorf("created status = %q, want queued", created.Status)
	}

	var got receivedCallback
	select {
	case got = <-callbacks:
	case <-time.After(10 * time.Second):
		t.Fatalf("no callback received; stdout:\n%s", strings.Join(sp.logs.snapshot(), "\n"))
	}
	if got.method != http.MethodPost {
		t.Errorf("callback method = %q, want POST", got.method)
	}
	if got.event != callbackEventCompleted {
		t.Errorf("callback event = %q, want %q", got.event, callbackEventCompleted)
	}
	if got.delivery == "" {
		t.Errorf("callback delivery id is empty")
	}
	if want := signTestBody(key, got.body); got.signature != want {
		t.Errorf("callback signature = %q, want %q", got.signature, want)
	}

	var payload struct {
		Job struct {
			ID         string    `json:"id"`
			Status     JobStatus `json:"status"`
			Conclusion string    `json:"conclusion"`
			Reason     string    `json:"reason"`
		} `json:"job"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode callback body: %v", err)
	}
	if payload.Job.ID != created.ID {
		t.Errorf("callback job id = %q, want %q", payload.Job.ID, created.ID)
	}
	if payload.Job.Status != JobStatusCompleted {
		t.Errorf("callback job status = %q, want completed", payload.Job.Status)
	}
	if payload.Job.Conclusion != JobConclusionSucceeded {
		t.Errorf("callback job conclusion = %q, want succeeded", payload.Job.Conclusion)
	}

	// The delivery record becomes visible to the control interface.
	deadline := time.Now().Add(10 * time.Second)
	var records deliveriesResponse
	for {
		status, body = doJSON(t, http.MethodGet, sp.controlURL+controlJobsPrefix+created.ID+"/"+controlDeliveries, nil)
		if status != http.StatusOK {
			t.Fatalf("GET deliveries = %d, want 200: %s", status, body)
		}
		if err := json.Unmarshal(body, &records); err != nil {
			t.Fatalf("decode deliveries: %v", err)
		}
		if len(records.Deliveries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery record appeared; stdout:\n%s", strings.Join(sp.logs.snapshot(), "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	record := records.Deliveries[0]
	if record.DeliveryID != got.delivery {
		t.Errorf("recorded delivery id = %q, want the callback header value %q", record.DeliveryID, got.delivery)
	}
	if record.Attempt != 1 {
		t.Errorf("recorded attempt = %d, want 1", record.Attempt)
	}
	if record.StatusCode != http.StatusOK {
		t.Errorf("recorded status = %d, want 200", record.StatusCode)
	}
	if record.Error != "" {
		t.Errorf("recorded error = %q, want empty", record.Error)
	}
	if _, err := time.Parse(time.RFC3339, record.SentAt); err != nil {
		t.Errorf("recorded sent_at = %q, want RFC 3339: %v", record.SentAt, err)
	}

	// The job query shows the final state.
	deadline = time.Now().Add(10 * time.Second)
	var final Job
	for {
		status, body = doJSON(t, http.MethodGet, sp.jobsURL+"/v1/jobs/"+created.ID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET job = %d, want 200: %s", status, body)
		}
		final = decodeJob(t, body)
		if final.Status == JobStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stayed %s", created.ID, final.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final.Conclusion != JobConclusionSucceeded {
		t.Errorf("final conclusion = %q, want succeeded", final.Conclusion)
	}

	// Every status change and the callback attempt each wrote one line.
	for _, want := range []string{
		"job=" + created.ID + " status=queued",
		"job=" + created.ID + " status=running",
		"job=" + created.ID + " status=completed",
		`msg="callback attempt"`,
	} {
		sp.waitForLog(t, want)
	}

	sp.stop(t)
}

// TestServeRejectsNonLoopbackControl checks that serve refuses to start when
// the control interface would not be bound to loopback.
func TestServeRejectsNonLoopbackControl(t *testing.T) {
	keyPath := writeTestKey(t, "key\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, extsimTestBinary,
		"serve",
		"--listen", "127.0.0.1:0",
		"--control-listen", "0.0.0.0:0",
		"--signing-key-file", keyPath,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("serve started with a non-loopback control address; output:\n%s", out)
	}
	if !strings.Contains(string(out), "loopback") {
		t.Errorf("output does not explain the loopback requirement:\n%s", out)
	}
}
