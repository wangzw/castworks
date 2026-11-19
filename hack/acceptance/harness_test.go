//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

const (
	// acceptanceRoot is outside the repository. Every run gets a timestamped
	// directory below it, and the ledger sits directly in it.
	acceptanceRoot = "/tmp/castworks-acceptance"
	ledgerFileName = "ledger"

	envDatabaseURL = "CASTWORKS_TEST_DATABASE_URL"
	envKubeContext = "CASTWORKS_ACC_KUBE_CONTEXT"

	ateSpace = "castworks-dev"

	defaultShutdownTimeout = 10 * time.Second
	pollInterval           = 250 * time.Millisecond
	httpTimeout            = 30 * time.Second
	actorGoneTimeout       = 2 * time.Minute
	// cleanupKubectlTimeout bounds each kubectl call cleanup makes directly.
	// Cleanup runs with a context that has no deadline, so without this a
	// wedged API server would hang the test until go test's own timeout.
	cleanupKubectlTimeout = 30 * time.Second
)

// loggedAddr matches the addr attribute of a slog text line. slog writes it
// unquoted unless it needs quoting; the quoted form is accepted too.
var loggedAddr = regexp.MustCompile(`\baddr="?([^"\s]+)"?`)

// ledgerEntry is one line of the ledger. A create entry carries the run ID,
// working directory and provider; a process entry carries a child PID. The
// ledger lets acceptance-clean remove a run and stop its processes after the
// test process was killed before t.Cleanup could run.
type ledgerEntry struct {
	Time     time.Time `json:"time"`
	RunID    string    `json:"run_id,omitempty"`
	WorkDir  string    `json:"work_dir,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Process  string    `json:"process,omitempty"`
	PID      int       `json:"pid,omitempty"`
}

var (
	ledgerMu sync.Mutex

	runRootOnce sync.Once
	runRootPath string
	runRootErr  error
)

func ledgerPath() string {
	return filepath.Join(acceptanceRoot, ledgerFileName)
}

// appendLedger appends one line to the ledger, creating its directory first.
func appendLedger(entry ledgerEntry) error {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()

	if err := os.MkdirAll(acceptanceRoot, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(ledgerPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(line, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func readLedger(path string) ([]ledgerEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []ledgerEntry
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry ledgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("ledger line %d: %w", i+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// runRoot returns this process's timestamped working root below
// acceptanceRoot. It does not depend on the CPU count or on any repository
// path. The directory is owner-only, as if created under umask 077: the run
// holds model output and raw traces.
func runRoot(t testing.TB) string {
	t.Helper()
	runRootOnce.Do(func() {
		stamp := time.Now().UTC().Format("20060102T150405Z")
		candidate := filepath.Join(acceptanceRoot, stamp)
		if err := os.MkdirAll(candidate, 0o700); err != nil {
			runRootErr = err
			return
		}
		runRootPath = candidate
	})
	if runRootErr != nil {
		t.Fatalf("acceptance: create working root: %v", runRootErr)
	}
	return runRootPath
}

func sanitizeTestName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "test"
	}
	return b.String()
}

// repoRoot returns the module root. The test file lives at
// <root>/hack/acceptance/harness_test.go.
func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("acceptance: cannot locate the repository root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// harnessOptions carries what a test needs before the harness starts anything.
type harnessOptions struct {
	// DatabaseURL is passed to the control plane as --database-url.
	DatabaseURL string
	// Provider is "local" or "substrate"; it decides whether cleanup talks to
	// the cluster.
	Provider string
}

type harness struct {
	t        *testing.T
	repoRoot string
	workDir  string
	binDir   string
	httpAddr string
	grpcAddr string
	provider string
	kubeCtx  string
	shutdown time.Duration
	database string
	client   *http.Client
	runID    string
	control  *childProcess
	warden   *childProcess

	// hooks replaces cleanup's side-effecting steps in tests that only check
	// their order. A nil hooks or nil field means the real implementation.
	hooks *cleanupHooks
}

// cleanupHooks lets a test observe and replace the side-effecting steps of
// cleanup. Real runs leave it nil.
type cleanupHooks struct {
	saveActor     func(ctx context.Context, actor string)
	endRun        func(ctx context.Context, runID string) error
	waitActorGone func(ctx context.Context, kubeContext, actor string, timeout time.Duration) error
	stopChild     func(child *childProcess, timeout time.Duration) error
}

// newHarness creates a per-test working directory and registers cleanup. The
// cleanup runs before any cleanup registered earlier, which keeps the
// ordering with pgtest's database drop correct.
func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	root := runRoot(t)
	workDir := filepath.Join(root, sanitizeTestName(t.Name()))
	// Owner-only, as if created under umask 077: the per-test directory holds
	// the home directory, strace output and copied samples.
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("acceptance: create work directory %s: %v", workDir, err)
	}

	shutdown := defaultShutdownTimeout
	h := &harness{
		t:        t,
		repoRoot: repoRoot(t),
		workDir:  workDir,
		provider: opts.Provider,
		kubeCtx:  optionalEnv(envKubeContext),
		shutdown: shutdown,
		database: opts.DatabaseURL,
		client:   &http.Client{Timeout: httpTimeout},
	}
	t.Cleanup(h.cleanup)
	return h
}

// buildBinaries builds the three real binaries into the working directory.
func (h *harness) buildBinaries() {
	h.t.Helper()
	binDir := filepath.Join(h.workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		h.t.Fatalf("acceptance: create binary directory: %v", err)
	}

	targets := []struct {
		name string
		pkg  string
	}{
		{"castworks-controlplane", "./cmd/castworks-controlplane"},
		{"warden", "./cmd/warden"},
		{"castworks", "./cmd/castworks"},
	}
	for _, target := range targets {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, target.name), target.pkg)
		cmd.Dir = h.repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.t.Fatalf("acceptance: go build %s: %v\n%s", target.pkg, err, out)
		}
	}
	h.binDir = binDir
}

// childProcess is a long-running acceptance child. Pdeathsig makes the child
// receive SIGTERM if the test process dies without running cleanup.
type childProcess struct {
	name    string
	cmd     *exec.Cmd
	waitErr error
	done    chan struct{}
}

func (c *childProcess) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// childEnvNames lists the only environment variables an acceptance child
// inherits. The control plane and warden are started with an explicit
// environment instead of os.Environ(): an LLM credential exported in the
// developer's shell must never leak into a child process.
var childEnvNames = []string{"PATH", "HOME", "TMPDIR"}

// childEnv returns the environment for an acceptance child: the variables in
// childEnvNames that are set in the test process, and nothing else.
func childEnv() []string {
	env := make([]string, 0, len(childEnvNames))
	for _, name := range childEnvNames {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// childCommand builds the command for a long-running acceptance child. The
// child runs in its own process group (Setpgid) so that a terminal SIGINT
// reaches only the test process: TestMain turns the first signal into context
// cancellation, and cleanup stops each child in order. Pdeathsig is the
// fallback for a test process that dies without running cleanup.
//
// It is separate from startChild so that a test can inspect the environment
// and the process attributes without starting anything.
func childCommand(bin string, args []string, stdout, stderr io.Writer) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = childEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
		Setpgid:   true,
	}
	return cmd
}

// startChild starts bin in the background, sending stdout and stderr to
// separate log files under the working directory.
func (h *harness) startChild(name, bin string, args []string) *childProcess {
	h.t.Helper()

	stdoutPath := filepath.Join(h.workDir, name+".stdout.log")
	stderrPath := filepath.Join(h.workDir, name+".stderr.log")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		h.t.Fatalf("acceptance: create %s: %v", stdoutPath, err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		_ = stdout.Close()
		h.t.Fatalf("acceptance: create %s: %v", stderrPath, err)
	}

	cmd := childCommand(bin, args, stdout, stderr)

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		h.t.Fatalf("acceptance: start %s: %v", name, err)
	}
	_ = stdout.Close()
	_ = stderr.Close()

	child := &childProcess{name: name, cmd: cmd, done: make(chan struct{})}
	go func() {
		child.waitErr = cmd.Wait()
		close(child.done)
	}()

	entry := ledgerEntry{
		Time:     time.Now().UTC(),
		RunID:    h.runID,
		WorkDir:  h.workDir,
		Provider: h.provider,
		Process:  name,
		PID:      cmd.Process.Pid,
	}
	if err := appendLedger(entry); err != nil {
		_ = cmd.Process.Kill()
		h.t.Fatalf("acceptance: record %s pid in ledger: %v", name, err)
	}
	return child
}

// waitChild waits for a child to exit and returns its wait error.
func (h *harness) waitChild(child *childProcess, timeout time.Duration) error {
	if child == nil {
		return nil
	}
	select {
	case <-child.done:
		return child.waitErr
	case <-time.After(timeout):
		return fmt.Errorf("%s did not exit within %s", child.name, timeout)
	}
}

// isProcessGone reports whether err means the target has already exited.
func isProcessGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// signalProcess signals pid and, when pid leads its own process group (as the
// harness starts every child), the whole group, so grandchildren such as the
// Claude Code process under warden receive the signal too.
func signalProcess(pid int, sig syscall.Signal) error {
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		target = -pid
	}
	return syscall.Kill(target, sig)
}

// stopChild asks a child's process group to exit with SIGTERM, waits, and
// sends SIGKILL when it overruns. It reports an error when the child had
// already failed or had to be killed.
func (h *harness) stopChild(child *childProcess, timeout time.Duration) error {
	if child == nil {
		return nil
	}
	if child.exited() {
		return child.waitErr
	}
	if err := signalProcess(child.cmd.Process.Pid, syscall.SIGTERM); err != nil && !isProcessGone(err) {
		return fmt.Errorf("signal %s: %w", child.name, err)
	}
	select {
	case <-child.done:
		return child.waitErr
	case <-time.After(timeout):
	}
	if err := signalProcess(child.cmd.Process.Pid, syscall.SIGKILL); err != nil && !isProcessGone(err) {
		return fmt.Errorf("kill %s: %w", child.name, err)
	}
	<-child.done
	return fmt.Errorf("%s did not exit within %s after SIGTERM; sent SIGKILL", child.name, timeout)
}

// TestWaitLoggedAddrReadsEveryLog covers the control plane writing its slog
// records to stderr while stdout stays empty: the harness must find the
// address either way, because the child's two streams go to separate files.
func TestWaitLoggedAddrReadsEveryLog(t *testing.T) {
	const (
		message = "http listening"
		line    = `time=2026-11-19T12:00:00.000Z level=INFO msg="http listening" addr=127.0.0.1:45678` + "\n"
		want    = "127.0.0.1:45678"
	)
	for _, tc := range []struct {
		name           string
		stdout, stderr string
	}{
		{name: "stderr only", stderr: line},
		{name: "stdout only", stdout: line},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{t: t, workDir: t.TempDir()}
			for _, stream := range []struct {
				name, content string
			}{
				{"controlplane.stdout.log", tc.stdout},
				{"controlplane.stderr.log", tc.stderr},
			} {
				path := filepath.Join(h.workDir, stream.name)
				if err := os.WriteFile(path, []byte(stream.content), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if got := h.waitLoggedAddr(ctx, message); got != want {
				t.Fatalf("waitLoggedAddr = %q, want %q", got, want)
			}
		})
	}
}

// TestDoSetsJSONContentTypeOnBodylessPost covers cleanup's end-run request:
// it carries no body, but the control plane's guard rejects any POST without
// application/json, so the header must be set for the method, not the body.
func TestDoSetsJSONContentTypeOnBodylessPost(t *testing.T) {
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	h := &harness{
		t:        t,
		httpAddr: strings.TrimPrefix(server.URL, "http://"),
		client:   server.Client(),
	}
	if err := h.do(context.Background(), http.MethodPost, "/v1/runs/x/end", nil, nil); err != nil {
		t.Fatalf("POST without a body: %v", err)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
}

// TestFakeClaudeEmitsSessionStateEvents covers the shape warden's Claude Code
// driver parses: state changes are system messages with subtype
// session_state_changed. A top-level session_state_changed message is ignored,
// which leaves the turn without its idle boundary.
func TestFakeClaudeEmitsSessionStateEvents(t *testing.T) {
	script := filepath.Join("testdata", "fake-claude", "fake-claude")
	cmd := exec.Command(script)
	cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run %s: %v", script, err)
	}

	var states []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if msg.State == "" {
			continue
		}
		if msg.Type != "system" || msg.Subtype != "session_state_changed" {
			t.Errorf("state %q sent as type %q subtype %q, want system/session_state_changed", msg.State, msg.Type, msg.Subtype)
		}
		states = append(states, msg.State)
	}
	if len(states) == 0 {
		t.Fatal("fake-claude emitted no session state events")
	}
}

// waitLoggedAddr waits for a control plane log line with the given message
// and returns its addr attribute. The control plane logs through slog, which
// writes to stderr; stdout is scanned as well so the lookup survives a change
// of the logging destination.
func (h *harness) waitLoggedAddr(ctx context.Context, message string) string {
	h.t.Helper()
	paths := []string{
		filepath.Join(h.workDir, "controlplane.stderr.log"),
		filepath.Join(h.workDir, "controlplane.stdout.log"),
	}
	addr, err := waitForAddr(ctx, paths, message)
	if err != nil {
		h.t.Fatalf("acceptance: read control plane address: %v", err)
	}
	return addr
}

// waitForAddr polls the log files until one holds a line containing message
// with an addr attribute, and returns that address.
func waitForAddr(ctx context.Context, paths []string, message string) (string, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		for _, path := range paths {
			if addr := loggedAddrIn(path, message); addr != "" {
				return addr, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for %q in %s: %w", message, strings.Join(paths, ", "), ctx.Err())
		case <-ticker.C:
		}
	}
}

// loggedAddrIn returns the addr attribute of the first line in path that
// contains message, or "" when the file cannot be read or has no such line.
func loggedAddrIn(path, message string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, message) {
			continue
		}
		if match := loggedAddr.FindStringSubmatch(line); match != nil {
			return match[1]
		}
	}
	return ""
}

// startControlPlane starts castworks-controlplane and waits until both
// listeners report their actual addresses.
func (h *harness) startControlPlane(ctx context.Context, args ...string) {
	h.t.Helper()
	full := args
	if h.database != "" {
		full = append(full, "--database-url", h.database)
	}
	h.control = h.startChild("controlplane", filepath.Join(h.binDir, "castworks-controlplane"), full)
	h.httpAddr = h.waitLoggedAddr(ctx, "http listening")
	h.grpcAddr = h.waitLoggedAddr(ctx, "grpc listening")
}

// startWardenLocal starts warden in local mode against the control plane.
func (h *harness) startWardenLocal(runID, claudeBin string) {
	h.t.Helper()
	args := []string{
		"--run-id", runID,
		"--control-plane", h.grpcAddr,
		"--home", filepath.Join(h.workDir, "home"),
		"--claude-bin", claudeBin,
	}
	h.warden = h.startChild("warden", filepath.Join(h.binDir, "warden"), args)
}

// httpStatusError is a non-2xx response from the control plane API.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Body)
}

func (h *harness) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://"+h.httpAddr+path, body)
	if err != nil {
		return err
	}
	// The control plane's guard requires application/json on every POST,
	// including bodyless ones such as /end, so set it for the method rather
	// than for a non-nil body.
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// turn mirrors the JSON shape humanchannel returns for one turn.
type turn struct {
	TurnID    string     `json:"turn_id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	Output    string     `json:"output"`
	IsError   bool       `json:"is_error"`
}

// suspension mirrors one suspension record. The timing fields are null while
// the step has not completed.
type suspension struct {
	StartedAt  time.Time `json:"started_at"`
	SnapshotMS *int64    `json:"snapshot_ms"`
	RestoreMS  *int64    `json:"restore_ms"`
	HelloMS    *int64    `json:"hello_ms"`
	Error      string    `json:"error"`
}

// runDetail mirrors GET /v1/runs/{run_id}.
type runDetail struct {
	RunID         string       `json:"run_id"`
	State         string       `json:"state"`
	Substate      string       `json:"substate"`
	FailureReason string       `json:"failure_reason"`
	Turns         []turn       `json:"turns"`
	Suspensions   []suspension `json:"suspensions"`
}

// createRun creates a run, records it in the ledger immediately, and returns
// its ID.
func (h *harness) createRun(ctx context.Context, prompt string) string {
	h.t.Helper()
	var response struct {
		RunID string `json:"run_id"`
	}
	if err := h.do(ctx, http.MethodPost, "/v1/runs", map[string]string{"prompt": prompt}, &response); err != nil {
		h.t.Fatalf("acceptance: create run: %v", err)
	}
	if response.RunID == "" {
		h.t.Fatal("acceptance: create run returned an empty run_id")
	}
	h.runID = response.RunID

	entry := ledgerEntry{
		Time:     time.Now().UTC(),
		RunID:    response.RunID,
		WorkDir:  h.workDir,
		Provider: h.provider,
	}
	if err := appendLedger(entry); err != nil {
		h.t.Fatalf("acceptance: record run %s in ledger: %v", response.RunID, err)
	}
	return response.RunID
}

func (h *harness) getRun(ctx context.Context, runID string) (runDetail, error) {
	var detail runDetail
	err := h.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &detail)
	return detail, err
}

func (h *harness) send(ctx context.Context, runID, text string) {
	h.t.Helper()
	path := "/v1/runs/" + url.PathEscape(runID) + "/messages"
	if err := h.do(ctx, http.MethodPost, path, map[string]string{"text": text}, nil); err != nil {
		h.t.Fatalf("acceptance: send message to run %s: %v", runID, err)
	}
}

func (h *harness) endRun(ctx context.Context, runID string) error {
	return h.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/end", nil, nil)
}

// waitFor polls check until it succeeds, an error occurs, or ctx expires.
func waitFor(ctx context.Context, description string, check func() (bool, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitTurnBoundary waits until at least the first n turns have ended and the
// run is back at a turn boundary.
func (h *harness) waitTurnBoundary(ctx context.Context, runID string, n int) runDetail {
	h.t.Helper()
	detail, err := h.waitTurnBoundaryErr(ctx, runID, n)
	if err != nil {
		h.t.Fatalf("acceptance: %v", err)
	}
	return detail
}

// waitTurnBoundaryErr is waitTurnBoundary without the fatal: it returns the
// latest run detail together with the error so an observation-only step can
// log the timeout and keep going.
func (h *harness) waitTurnBoundaryErr(ctx context.Context, runID string, n int) (runDetail, error) {
	var detail runDetail
	description := fmt.Sprintf("run %s to finish %d turn(s) and reach a boundary", runID, n)
	err := waitFor(ctx, description, func() (bool, error) {
		current, err := h.getRun(ctx, runID)
		if err != nil {
			return false, err
		}
		detail = current
		if current.State == "failed" {
			return false, fmt.Errorf("run %s failed: %s", runID, current.FailureReason)
		}
		if len(current.Turns) < n {
			return false, nil
		}
		for _, finished := range current.Turns[:n] {
			if finished.EndedAt == nil {
				return false, nil
			}
		}
		if current.Substate != "boundary" {
			return false, nil
		}
		return true, nil
	})
	return detail, err
}

// kubectlAte runs kubectl ate with the optional kube context.
func kubectlAte(ctx context.Context, kubeContext string, args ...string) (string, error) {
	full := make([]string, 0, len(args)+3)
	if kubeContext != "" {
		full = append(full, "--context", kubeContext)
	}
	full = append(full, "ate")
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if warning := strings.TrimSpace(stderr.String()); warning != "" {
			if detail != "" {
				detail += "\n"
			}
			detail += warning
		}
		return string(out), fmt.Errorf("kubectl %s: %w: %s", strings.Join(full, " "), err, detail)
	}
	return string(out), nil
}

func isActorMissing(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "notfound")
}

// waitActorGone waits until kubectl ate no longer reports the actor.
func waitActorGone(ctx context.Context, kubeContext, actor string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		_, err := kubectlAte(waitCtx, kubeContext, "get", "actor", actor, "-a", ateSpace, "-o", "json")
		switch {
		case err == nil:
			lastErr = fmt.Errorf("actor %s still exists", actor)
		case isActorMissing(err):
			return nil
		default:
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for actor %s to disappear: %w (last error: %v)", actor, waitCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// saveActorState writes the actor JSON and whatever logs are readable to the
// working directory before the actor is deleted.
func (h *harness) saveActorState(ctx context.Context, actor string) {
	h.t.Helper()

	// The caller may pass a context without a deadline, as cleanup does; bound
	// the kubectl calls here so neither one can hang forever.
	ctx, cancel := context.WithTimeout(ctx, cleanupKubectlTimeout)
	defer cancel()

	out, err := kubectlAte(ctx, h.kubeCtx, "get", "actor", actor, "-a", ateSpace, "-o", "json")
	if err != nil {
		h.t.Logf("cleanup: read actor %s: %v", actor, err)
	} else {
		path := filepath.Join(h.workDir, "actor-"+actor+".json")
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			h.t.Errorf("cleanup: write %s: %v", path, err)
		}
	}

	logs, err := kubectlAte(ctx, h.kubeCtx, "logs", "actor", actor, "-a", ateSpace)
	if err != nil {
		h.t.Logf("cleanup: read actor %s logs: %v", actor, err)
		return
	}
	path := filepath.Join(h.workDir, "actor-"+actor+".logs")
	if err := os.WriteFile(path, []byte(logs), 0o644); err != nil {
		h.t.Errorf("cleanup: write %s: %v", path, err)
	}
}

// cleanup ends the run, removes actors, stops the child processes, and keeps
// going after each failure so that everything is attempted. pgtest's database
// drop was registered before this function and therefore runs after it.
//
// The actor state is saved before End, because End removes the actor and its
// logs with it.
func (h *harness) cleanup() {
	saveActorState := h.saveActorState
	endRun := h.endRun
	waitActorGoneFn := waitActorGone
	stopChild := h.stopChild
	if h.hooks != nil {
		if h.hooks.saveActor != nil {
			saveActorState = h.hooks.saveActor
		}
		if h.hooks.endRun != nil {
			endRun = h.hooks.endRun
		}
		if h.hooks.waitActorGone != nil {
			waitActorGoneFn = h.hooks.waitActorGone
		}
		if h.hooks.stopChild != nil {
			stopChild = h.hooks.stopChild
		}
	}

	actor := ""
	if h.provider == "substrate" && h.runID != "" {
		actor = sandboxprovider.ActorName(h.runID)
		saveActorState(context.Background(), actor)
	}

	if h.runID != "" && h.httpAddr != "" && h.control != nil && !h.control.exited() {
		endCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := endRun(endCtx, h.runID)
		cancel()
		var statusErr *httpStatusError
		switch {
		case err == nil:
		case errors.As(err, &statusErr):
			h.t.Logf("cleanup: end run %s: %v", h.runID, err)
		default:
			h.t.Errorf("cleanup: end run %s: %v", h.runID, err)
		}
	}

	if actor != "" {
		if err := waitActorGoneFn(context.Background(), h.kubeCtx, actor, actorGoneTimeout); err != nil {
			h.t.Errorf("cleanup: %v; deleting the actor directly", err)
			deleteCtx, cancelDelete := context.WithTimeout(context.Background(), cleanupKubectlTimeout)
			_, deleteErr := kubectlAte(deleteCtx, h.kubeCtx, "delete", "actor", actor, "-a", ateSpace)
			cancelDelete()
			if deleteErr != nil {
				h.t.Errorf("cleanup: delete actor %s: %v", actor, deleteErr)
			}
		}
	}

	stopTimeout := h.shutdown + 5*time.Second
	if err := stopChild(h.warden, stopTimeout); err != nil {
		h.t.Errorf("cleanup: stop warden: %v", err)
	}
	if err := stopChild(h.control, stopTimeout); err != nil {
		h.t.Errorf("cleanup: stop control plane: %v", err)
	}
}

// substrateRunIDs returns the run IDs of substrate runs recorded in the
// ledger, in a stable order.
func substrateRunIDs(entries []ledgerEntry) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, entry := range entries {
		if entry.RunID == "" || entry.Provider != "substrate" {
			continue
		}
		if _, ok := seen[entry.RunID]; ok {
			continue
		}
		seen[entry.RunID] = struct{}{}
		ids = append(ids, entry.RunID)
	}
	sort.Strings(ids)
	return ids
}

// recordedProcesses returns the last process entry per PID. Later entries
// win, so a PID reused after a previous run is checked against its latest
// expected name.
func recordedProcesses(entries []ledgerEntry) []ledgerEntry {
	byPID := make(map[int]ledgerEntry)
	for _, entry := range entries {
		if entry.PID <= 0 || entry.Process == "" {
			continue
		}
		byPID[entry.PID] = entry
	}
	pids := make([]int, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	processes := make([]ledgerEntry, 0, len(pids))
	for _, pid := range pids {
		processes = append(processes, byPID[pid])
	}
	return processes
}

// expectedExecutable maps a ledger entry to the full executable path recorded
// for its child, or "" when the process name is unknown or the entry has no
// working directory. Cleanup matches this against argv[0] exactly: matching a
// substring of the command line would also hit an unrelated process that
// merely mentions the name in a path or an argument.
func expectedExecutable(entry ledgerEntry) string {
	var binary string
	switch entry.Process {
	case "controlplane":
		binary = "castworks-controlplane"
	case "warden":
		binary = "warden"
	default:
		return ""
	}
	if entry.WorkDir == "" {
		return ""
	}
	return filepath.Join(entry.WorkDir, "bin", binary)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// stopRecordedProcess sends SIGTERM to a PID from the ledger, but only after
// checking that the process's executable (argv[0]) is exactly the acceptance
// binary recorded for it.
func stopRecordedProcess(t *testing.T, entry ledgerEntry) {
	t.Helper()

	expected := expectedExecutable(entry)
	if expected == "" {
		t.Logf("clean: unknown process %q (or missing work directory) for pid %d; skipping", entry.Process, entry.PID)
		return
	}

	cmdlinePath := filepath.Join("/proc", strconv.Itoa(entry.PID), "cmdline")
	cmdline, err := os.ReadFile(cmdlinePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Errorf("clean: read %s: %v", cmdlinePath, err)
		return
	}
	// /proc/<pid>/cmdline joins argv with NUL bytes; the first field is the
	// executable the process was started with.
	argv0 := strings.SplitN(string(cmdline), "\x00", 2)[0]
	if argv0 != expected {
		t.Logf("clean: pid %d runs %q, not %q; skipping", entry.PID, argv0, expected)
		return
	}

	if err := signalProcess(entry.PID, syscall.SIGTERM); err != nil && !isProcessGone(err) {
		t.Errorf("clean: signal pid %d: %v", entry.PID, err)
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(entry.PID) {
			return
		}
		time.Sleep(pollInterval)
	}
	if err := signalProcess(entry.PID, syscall.SIGKILL); err != nil && !isProcessGone(err) {
		t.Errorf("clean: kill pid %d: %v", entry.PID, err)
	}
}

// TestClean removes what a killed run left behind: actors recorded for
// substrate runs and the child processes recorded in the ledger. It needs
// neither the database nor a control plane.
func TestClean(t *testing.T) {
	ctx := testContext(t, 5*time.Minute)
	kubeContext := optionalEnv(envKubeContext)

	entries, err := readLedger(ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		t.Logf("clean: no ledger at %s; nothing to clean", ledgerPath())
		return
	}
	if err != nil {
		t.Fatalf("clean: read ledger: %v", err)
	}

	for _, runID := range substrateRunIDs(entries) {
		actor := sandboxprovider.ActorName(runID)
		if _, err := kubectlAte(ctx, kubeContext, "get", "actor", actor, "-a", ateSpace, "-o", "json"); err != nil {
			if isActorMissing(err) {
				continue
			}
			t.Errorf("clean: get actor %s: %v", actor, err)
			continue
		}
		if out, err := kubectlAte(ctx, kubeContext, "delete", "actor", actor, "-a", ateSpace); err != nil {
			t.Errorf("clean: delete actor %s: %v", actor, err)
		} else {
			t.Logf("clean: deleted actor %s: %s", actor, strings.TrimSpace(out))
		}
		if err := waitActorGone(ctx, kubeContext, actor, actorGoneTimeout); err != nil {
			t.Errorf("clean: %v", err)
		}
	}

	for _, entry := range recordedProcesses(entries) {
		stopRecordedProcess(t, entry)
	}

	// The ledger only describes work that still needs cleaning. Once every
	// entry has been handled, truncate it so a later clean cannot act on a PID
	// that the OS has since reused. A failed step keeps the ledger so the
	// clean can be retried.
	if t.Failed() {
		return
	}
	if err := os.Truncate(ledgerPath(), 0); err != nil {
		t.Errorf("clean: truncate ledger: %v", err)
	}
}

// TestChildProcessIsolation checks that acceptance children do not inherit the
// test process's environment and run in their own process group. Both are
// safety properties: an exported LLM credential must not reach a child, and a
// terminal SIGINT must not skip the ordered shutdown.
func TestChildProcessIsolation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-api-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-oauth-token")

	cmd := childCommand("/nonexistent/bin", nil, io.Discard, io.Discard)

	env := make(map[string]string, len(cmd.Env))
	for _, entry := range cmd.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("environment entry %q has no =", entry)
		}
		env[name] = value
	}

	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if value, ok := env[name]; ok {
			t.Errorf("%s leaked into the child environment (value %q)", name, value)
		}
	}
	for _, name := range childEnvNames {
		want, wantSet := os.LookupEnv(name)
		got, gotSet := env[name]
		if gotSet != wantSet || got != want {
			t.Errorf("%s: child has (%q, %t), test process has (%q, %t)", name, got, gotSet, want, wantSet)
		}
	}

	if cmd.SysProcAttr == nil {
		t.Fatal("child command has no SysProcAttr")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Errorf("Pdeathsig = %v, want SIGTERM", cmd.SysProcAttr.Pdeathsig)
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid is false; a terminal SIGINT would reach the child directly")
	}
}

// TestCleanupSavesActorBeforeEnd checks the order of cleanup: End removes the
// actor, so its JSON and logs must be read first.
func TestCleanupSavesActorBeforeEnd(t *testing.T) {
	var steps []string
	h := &harness{
		t:        t,
		provider: "substrate",
		runID:    "run-under-test",
		httpAddr: "127.0.0.1:1",
		control:  &childProcess{name: "controlplane", done: make(chan struct{})},
		shutdown: time.Second,
		hooks: &cleanupHooks{
			saveActor: func(context.Context, string) {
				steps = append(steps, "save-actor")
			},
			endRun: func(context.Context, string) error {
				steps = append(steps, "end-run")
				return nil
			},
			waitActorGone: func(context.Context, string, string, time.Duration) error {
				steps = append(steps, "wait-actor-gone")
				return nil
			},
			stopChild: func(child *childProcess, _ time.Duration) error {
				if child != nil {
					steps = append(steps, "stop-"+child.name)
				}
				return nil
			},
		},
	}

	h.cleanup()

	want := []string{"save-actor", "end-run", "wait-actor-gone", "stop-controlplane"}
	if !slices.Equal(steps, want) {
		t.Fatalf("cleanup steps = %v, want %v", steps, want)
	}
}

// TestStopRecordedProcessMatchesExecutablePath checks that cleaning by PID
// compares argv[0] with the recorded executable path instead of matching a
// substring: a process whose path merely contains the name must survive.
func TestStopRecordedProcessMatchesExecutablePath(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is needed to exercise process matching")
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create bin directory: %v", err)
	}

	// startSleeper runs sleep with argv[0] set to argv0, which is exactly what
	// /proc/<pid>/cmdline reports once the child has finished exec.
	startSleeper := func(argv0 string) (*exec.Cmd, <-chan struct{}) {
		cmd := exec.Command(sleep, "60")
		cmd.Args[0] = argv0
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleeper with argv[0] %q: %v", argv0, err)
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-done
		})

		// /proc/<pid>/cmdline is empty between fork and exec; wait for the
		// first argv field so the test reads the executable the process runs.
		cmdlinePath := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "cmdline")
		deadline := time.Now().Add(5 * time.Second)
		for {
			data, err := os.ReadFile(cmdlinePath)
			if err == nil && len(data) > 0 && data[0] != 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pid %d did not finish exec: %v", cmd.Process.Pid, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return cmd, done
	}

	exactCmd, exactDone := startSleeper(filepath.Join(binDir, "warden"))
	stopRecordedProcess(t, ledgerEntry{Process: "warden", PID: exactCmd.Process.Pid, WorkDir: workDir})
	select {
	case <-exactDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the recorded binary was not stopped")
	}

	lookalikeCmd, lookalikeDone := startSleeper(filepath.Join(binDir, "warden-old"))
	stopRecordedProcess(t, ledgerEntry{Process: "warden", PID: lookalikeCmd.Process.Pid, WorkDir: workDir})
	select {
	case <-lookalikeDone:
		t.Fatal("a process whose path merely contains the name was stopped")
	case <-time.After(2 * time.Second):
	}
	_ = lookalikeCmd.Process.Kill()
	<-lookalikeDone
}
