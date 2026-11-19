//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/sandboxprovider"
	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

const (
	// substrateGRPCAddr is the address the actors dial back to. The integration
	// tests that need a callback use the same address, so the two must never
	// run at the same time.
	substrateGRPCAddr = "10.12.72.11:9090"

	// substratePoolNamespace holds the WorkerPool the actor template selects.
	substratePoolNamespace = "castworks-system"

	envCredentialFile = "CASTWORKS_ACC_CREDENTIAL_FILE"
	envActorImage     = "CASTWORKS_ACC_ACTOR_IMAGE"

	// The actor template asks for exactly these limits. A worker can accept the
	// actor only when its free capacity covers both.
	actorTemplateCPUMillicores = 2000
	actorTemplateMemoryBytes   = 2 << 30

	// substrateTestTimeout stays five minutes below the make target's
	// -timeout 30m so a deadline fails the test and runs t.Cleanup instead of
	// letting go test panic.
	substrateTestTimeout = 25 * time.Minute

	// The cold start interval is shorter than pollInterval: the end-to-end
	// cold start is the time until the run first leaves provisioning, and a
	// coarse sample would round that measurement up.
	provisioningPollInterval = 100 * time.Millisecond

	actorLogWaitTimeout = 90 * time.Second
)

// Run states as humanchannel returns them. The acceptance package does not
// import runcontroller, so the values are repeated here.
const (
	runStateProvisioning = "provisioning"
	runStateSuspending   = "suspending"
	runStateFailed       = "failed"
)

var actorImageRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// TestSubstrateTwoTurns is the Substrate acceptance step: the control plane
// runs with --provider substrate and without --suspend, an actor completes two
// turns, and End removes the actor.
//
// The first prompt makes Claude Code write a file holding a random marker and
// run ps, so the reply carries the actor's process tree. The second prompt
// refers to the file only as "that file", which proves the session survived
// the first turn.
func TestSubstrateTwoTurns(t *testing.T) {
	ctx := testContext(t, substrateTestTimeout)

	requireEnv(t, envDatabaseURL)
	credentialPath := requireEnv(t, envCredentialFile)
	actorImage := requireEnv(t, envActorImage)
	if !actorImageRef.MatchString(actorImage) {
		t.Fatalf("acceptance: %s is %q, want name@sha256:<64 hex digits>", envActorImage, actorImage)
	}
	kubeContext := optionalEnv(envKubeContext)

	// Every precondition is checked before a process starts.
	checkSubstratePreconditions(t, ctx, kubeContext)

	pool := pgtest.NewPool(t)
	h := newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "substrate",
	})
	h.buildBinaries()

	args := []string{
		"--provider", "substrate",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", substrateGRPCAddr,
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialPath,
		"--substrate-image", actorImage,
		"--shutdown-timeout", "10s",
	}
	if kubeContext != "" {
		args = append(args, "--substrate-context", kubeContext)
	}
	h.startControlPlane(ctx, args...)

	marker := "substrate-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	prompt := fmt.Sprintf(
		"在工作目录里创建文件 note.txt，内容只有 %s，不要换行。然后执行 `ps -eo pid,ppid,user,comm`，把输出原样放进代码块，不要改成表格。",
		marker,
	)
	runID := h.createRun(ctx, prompt)
	requestReturned := time.Now()
	actor := sandboxprovider.ActorName(runID)
	t.Logf("acceptance: run %s maps to actor %s", runID, actor)

	monitor := startRunStateMonitor(ctx, h, runID)
	t.Cleanup(monitor.stop)

	// The actor logs are only readable while the actor runs, so they are read
	// during the first turn rather than after End.
	h.captureActorLogsDuringTurn(ctx, runID, actor)

	first := h.waitTurnBoundary(ctx, runID, 1)
	if len(first.Turns) < 1 {
		t.Fatalf("acceptance: run %s has no first turn: %+v", runID, first)
	}
	if first.Turns[0].IsError {
		t.Fatalf("acceptance: first turn is an error: %q", first.Turns[0].Output)
	}
	checkWardenProcessTree(t, first.Turns[0].Output)

	// Record the cold start now, while the measurement is complete. Waiting
	// for the second turn would throw it away if that turn failed.
	recordSubstrateColdStart(t, h, runID, requestReturned, monitor)

	// The second message must not repeat the file name or the marker: only the
	// session can supply them.
	h.send(ctx, runID, "把刚才那个文件的内容原样回复出来。")

	second := h.waitTurnBoundary(ctx, runID, 2)
	if len(second.Turns) != 2 {
		t.Fatalf("acceptance: expected 2 turns, got %d: %+v", len(second.Turns), second)
	}
	for i, finished := range second.Turns {
		if finished.IsError {
			t.Fatalf("acceptance: turn %d is an error: %q", i+1, finished.Output)
		}
	}
	if !strings.Contains(second.Turns[1].Output, marker) {
		t.Fatalf("acceptance: second turn output %q does not contain the marker %q", second.Turns[1].Output, marker)
	}
	if len(second.Suspensions) != 0 {
		t.Fatalf("acceptance: run %s recorded %d suspensions although --suspend was not given", runID, len(second.Suspensions))
	}

	// The actor JSON and its logs are gone once the actor is deleted, so they
	// are saved before End.
	h.saveActorState(ctx, actor)

	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("acceptance: end run %s: %v", runID, err)
	}
	if err := waitActorGone(ctx, h.kubeCtx, actor, actorGoneTimeout); err != nil {
		t.Fatalf("acceptance: actor %s was not removed after End: %v", actor, err)
	}
	t.Logf("acceptance: actor %s is gone after End", actor)

	monitor.stop()
	if monitor.sawSuspending() {
		t.Errorf("acceptance: run %s entered %q although --suspend was not given", runID, runStateSuspending)
	}
}

// checkSubstratePreconditions verifies the cluster is ready without starting
// anything: the callback address is free, the ledger lists no actor that is
// still around, and the WorkerPool can hold one more actor.
func checkSubstratePreconditions(t *testing.T, ctx context.Context, kubeContext string) {
	t.Helper()
	checkSubstrateAddrAvailable(t)
	checkNoLedgerActors(t, ctx, kubeContext)
	checkIdleSubstrateWorker(t, ctx, kubeContext)
}

// checkSubstrateAddrAvailable fails when another control plane or integration
// test already listens on the callback address.
func checkSubstrateAddrAvailable(t *testing.T) {
	t.Helper()

	listener, err := net.Listen("tcp", substrateGRPCAddr)
	if err != nil {
		t.Fatalf(
			"acceptance: cannot listen on %s: %v; another control plane or an integration test may be using it, and they must not run at the same time",
			substrateGRPCAddr, err,
		)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("acceptance: close the probe listener on %s: %v", substrateGRPCAddr, err)
	}
}

// substrateActorList mirrors the protojson shape of ListActorsResponse. Names
// are all the precondition needs.
type substrateActorList struct {
	Actors []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"actors"`
}

// checkNoLedgerActors fails when an actor recorded in the ledger still exists
// in the atespace. A leftover actor means a previous run was killed before its
// cleanup, and acceptance-clean has not been run since.
func checkNoLedgerActors(t *testing.T, ctx context.Context, kubeContext string) {
	t.Helper()

	entries, err := readLedger(ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("acceptance: read ledger %s: %v", ledgerPath(), err)
	}
	runIDs := substrateRunIDs(entries)
	if len(runIDs) == 0 {
		return
	}

	out, err := kubectlAte(ctx, kubeContext, "get", "actors", "-a", ateSpace, "-o", "json")
	if err != nil {
		t.Fatalf("acceptance: list actors in %s: %v", ateSpace, err)
	}
	var list substrateActorList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("acceptance: decode actors from %s: %v", ateSpace, err)
	}
	present := make(map[string]struct{}, len(list.Actors))
	for _, entry := range list.Actors {
		present[entry.Metadata.Name] = struct{}{}
	}
	for _, runID := range runIDs {
		actor := sandboxprovider.ActorName(runID)
		if _, ok := present[actor]; ok {
			t.Fatalf(
				"acceptance: actor %s from the ledger still exists in %s; run make acceptance-clean first",
				actor, ateSpace,
			)
		}
	}
}

// substrateWorkerResources mirrors the fields of WorkerResources that decide
// placement. protojson encodes int64 fields as strings ("cpuMillicores":
// "8000"), so a plain int64 tag would fail to unmarshal; the ,string option
// accepts the quoted protojson form.
type substrateWorkerResources struct {
	CPUMillicores int64 `json:"cpuMillicores,string"`
	MemoryBytes   int64 `json:"memoryBytes,string"`
}

// substrateWorkerList mirrors ListWorkersResponse.
type substrateWorkerList struct {
	Workers []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		WorkerPool string `json:"workerPool"`
		Status     struct {
			State     string                   `json:"state"`
			Capacity  substrateWorkerResources `json:"capacity"`
			Allocated substrateWorkerResources `json:"allocated"`
		} `json:"status"`
	} `json:"workers"`
}

// checkIdleSubstrateWorker fails unless one active worker has room for the
// actor template. Placement compares the CPU and memory limits the template
// asks for, so a WorkerPool whose free capacity is too small fails even when
// no actor is running.
func checkIdleSubstrateWorker(t *testing.T, ctx context.Context, kubeContext string) {
	t.Helper()

	out, err := kubectlAte(ctx, kubeContext, "get", "workers", "-n", substratePoolNamespace, "-o", "json")
	if err != nil {
		t.Fatalf("acceptance: list workers in %s: %v", substratePoolNamespace, err)
	}
	var list substrateWorkerList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("acceptance: decode workers in %s: %v", substratePoolNamespace, err)
	}

	details := make([]string, 0, len(list.Workers))
	for _, worker := range list.Workers {
		freeCPU := worker.Status.Capacity.CPUMillicores - worker.Status.Allocated.CPUMillicores
		freeMemory := worker.Status.Capacity.MemoryBytes - worker.Status.Allocated.MemoryBytes
		details = append(details, fmt.Sprintf(
			"%s state=%s cpu=%d/%d memory=%d/%d",
			worker.Metadata.Name,
			worker.Status.State,
			worker.Status.Allocated.CPUMillicores, worker.Status.Capacity.CPUMillicores,
			worker.Status.Allocated.MemoryBytes, worker.Status.Capacity.MemoryBytes,
		))
		if worker.Status.State != "WORKER_STATE_ACTIVE" {
			continue
		}
		if freeCPU < actorTemplateCPUMillicores || freeMemory < actorTemplateMemoryBytes {
			continue
		}
		return
	}

	if len(details) == 0 {
		t.Fatalf("acceptance: WorkerPool %s has no worker", substratePoolNamespace)
	}
	t.Fatalf(
		"acceptance: no active worker in WorkerPool %s has room for the actor template (%d millicores, %d bytes): %s",
		substratePoolNamespace, actorTemplateCPUMillicores, actorTemplateMemoryBytes,
		strings.Join(details, "; "),
	)
}

// captureActorLogsDuringTurn waits for the first turn to appear and reads the
// actor logs while the actor still runs, saving them in the working directory.
// The logs disappear with the actor, so this is the only chance to see warden's
// own output from the running actor. The wait only requires that the turn
// exists: a turn that ended between two polls must not keep this waiting until
// the test deadline.
func (h *harness) captureActorLogsDuringTurn(ctx context.Context, runID, actor string) {
	h.t.Helper()

	err := waitFor(ctx, fmt.Sprintf("run %s to start its first turn", runID), func() (bool, error) {
		detail, err := h.getRun(ctx, runID)
		if err != nil {
			return false, err
		}
		if detail.State == runStateFailed {
			return false, fmt.Errorf("run %s failed: %s", runID, detail.FailureReason)
		}
		return len(detail.Turns) > 0, nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v", err)
	}

	logCtx, cancel := context.WithTimeout(ctx, actorLogWaitTimeout)
	defer cancel()

	var logs string
	err = waitFor(logCtx, fmt.Sprintf("logs of actor %s during the first turn", actor), func() (bool, error) {
		out, err := kubectlAte(logCtx, h.kubeCtx, "logs", "actor", actor, "-a", ateSpace)
		if err != nil || strings.TrimSpace(out) == "" {
			// The warden may not have written anything yet, and the actor may
			// still be starting. Keep polling until the deadline.
			return false, nil
		}
		logs = out
		return true, nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: read logs of actor %s during the first turn: %v", actor, err)
	}

	path := filepath.Join(h.workDir, "actor-"+actor+".turn.logs")
	if err := os.WriteFile(path, []byte(logs), 0o644); err != nil {
		h.t.Fatalf("acceptance: write actor logs to %s: %v", path, err)
	}
	h.t.Logf("acceptance: saved logs of actor %s during the first turn to %s (%d bytes)", actor, path, len(logs))
}

// procRow is one row of ps -eo pid,ppid,user,comm output.
type procRow struct {
	pid  int
	ppid int
	user string
	comm string
}

// procMarkup strips the decoration a model adds when it formats command
// output: Markdown table pipes and fenced code block backticks. Removing them
// leaves the same whitespace-separated row the command printed.
var procMarkup = strings.NewReplacer("|", " ", "`", " ")

// parseProcRows turns ps output into rows. Header lines, Markdown separator
// rows and any other line whose first two fields are not numbers are skipped.
func parseProcRows(output string) []procRow {
	var rows []procRow
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(procMarkup.Replace(line))
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		rows = append(rows, procRow{pid: pid, ppid: ppid, user: fields[2], comm: fields[3]})
	}
	return rows
}

// checkWardenProcessTree reads the ps output from the first turn. Inside the
// actor, warden is PID 1 and runs as agent; Claude Code is its child.
func checkWardenProcessTree(t *testing.T, output string) {
	t.Helper()

	rows := parseProcRows(output)
	var init *procRow
	for i := range rows {
		if rows[i].pid == 1 {
			init = &rows[i]
			break
		}
	}
	if init == nil {
		t.Fatalf("acceptance: the ps output from the first turn has no row for PID 1:\n%s", output)
	}
	if init.user != "agent" {
		t.Errorf("acceptance: PID 1 runs as user %q, want %q:\n%s", init.user, "agent", output)
	}
	if !strings.Contains(strings.ToLower(init.comm), "warden") {
		t.Errorf("acceptance: PID 1 command is %q, want warden:\n%s", init.comm, output)
	}

	foundClaude := false
	for _, row := range rows {
		if row.ppid == 1 && isClaudeCodeComm(row.comm) {
			foundClaude = true
			break
		}
	}
	if !foundClaude {
		t.Fatalf("acceptance: no Claude Code process is a child of PID 1 in the ps output:\n%s", output)
	}
}

// isClaudeCodeComm matches the ps comm of Claude Code. The npm installation
// runs it under node, so the kernel reports either name.
func isClaudeCodeComm(comm string) bool {
	lower := strings.ToLower(comm)
	return strings.Contains(lower, "claude") || lower == "node" || lower == "nodejs"
}

// TestSubstrateParsesFormattedProcessTree covers the ps output shapes the model
// returns: a plain listing, one wrapped in a fenced code block, and a Markdown
// table. A table used to parse as no rows at all, so a correct process tree
// failed only because the model reformatted it.
func TestSubstrateParsesFormattedProcessTree(t *testing.T) {
	const listing = `  PID  PPID USER  COMM
    1     0 agent warden
   42     1 agent node`
	const fenced = "```\n" + listing + "\n```"
	const table = `| PID | PPID | USER | COMM |
| --- | --- | --- | --- |
| 1 | 0 | agent | warden |
| 42 | 1 | agent | node |`

	for name, output := range map[string]string{
		"plain":  listing,
		"fenced": fenced,
		"table":  table,
	} {
		t.Run(name, func(t *testing.T) {
			rows := parseProcRows(output)
			if len(rows) != 2 {
				t.Fatalf("parsed %d rows, want 2: %+v", len(rows), rows)
			}
			if rows[0].pid != 1 || rows[0].ppid != 0 || rows[0].user != "agent" || rows[0].comm != "warden" {
				t.Fatalf("first row = %+v, want PID 1 as agent running warden", rows[0])
			}
			if rows[1].pid != 42 || rows[1].ppid != 1 || rows[1].comm != "node" {
				t.Fatalf("second row = %+v, want PID 42 under PID 1 running node", rows[1])
			}
			checkWardenProcessTree(t, output)
		})
	}
}

// substrateColdStart records the two cold start measurements the milestone
// asks for: the provider's own Provision duration and the end-to-end time from
// the create response until the run first leaves provisioning.
type substrateColdStart struct {
	RunID              string `json:"run_id"`
	ProvisionMS        int64  `json:"provision_ms"`
	EndToEndMS         int64  `json:"end_to_end_ms"`
	EndToEndDefinition string `json:"end_to_end_definition"`
}

const endToEndColdStartDefinition = "POST /v1/runs returned to the run first leaving provisioning"

// recordSubstrateColdStart writes coldstart.json in the working directory and
// logs both values. The provision line comes from runcontroller's fixed
// `provider call` log record.
func recordSubstrateColdStart(t *testing.T, h *harness, runID string, requestReturned time.Time, monitor *runStateMonitor) {
	t.Helper()

	provisionMS, ok := provisionDurationMS(h.workDir, runID)
	if !ok {
		t.Fatalf("acceptance: control plane logs have no successful `provider call` line for provision of run %s", runID)
	}
	left := monitor.firstLeftProvisioning()
	if left.IsZero() {
		t.Fatalf("acceptance: never observed run %s leaving provisioning", runID)
	}

	record := substrateColdStart{
		RunID:              runID,
		ProvisionMS:        provisionMS,
		EndToEndMS:         left.Sub(requestReturned).Milliseconds(),
		EndToEndDefinition: endToEndColdStartDefinition,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("acceptance: encode cold start record: %v", err)
	}
	path := filepath.Join(h.workDir, "coldstart.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("acceptance: write %s: %v", path, err)
	}
	t.Logf("acceptance: cold start recorded in %s: provision=%dms end-to-end=%dms", path, record.ProvisionMS, record.EndToEndMS)
}

// attrPattern matches a slog text attribute, quoted or not.
var attrPattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*"|\S*)`)

// provisionDurationMS finds runcontroller's `provider call` line for the
// successful Provision of runID and returns its duration_ms value.
func provisionDurationMS(workDir, runID string) (int64, bool) {
	for _, name := range []string{"controlplane.stderr.log", "controlplane.stdout.log"} {
		data, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "provider call") {
				continue
			}
			attrs := logAttributes(line)
			if attrs["op"] != "provision" || attrs["run_id"] != runID || attrs["error"] != "" {
				continue
			}
			ms, err := strconv.ParseInt(attrs["duration_ms"], 10, 64)
			if err != nil {
				continue
			}
			return ms, true
		}
	}
	return 0, false
}

// logAttributes extracts the key=value pairs from a slog text line.
func logAttributes(line string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range attrPattern.FindAllStringSubmatch(line, -1) {
		value := match[2]
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		}
		attrs[match[1]] = value
	}
	return attrs
}

// runStateMonitor samples the run while it lives. It records when the run
// first left provisioning (the end-to-end cold start) and whether the state
// ever became suspending, which must not happen without --suspend.
type runStateMonitor struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu               sync.Mutex
	leftProvisioning time.Time
	suspending       bool
}

func startRunStateMonitor(parent context.Context, h *harness, runID string) *runStateMonitor {
	ctx, cancel := context.WithCancel(parent)
	m := &runStateMonitor{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(provisioningPollInterval)
		defer ticker.Stop()
		for {
			m.observe(ctx, h, runID)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return m
}

func (m *runStateMonitor) observe(ctx context.Context, h *harness, runID string) {
	detail, err := h.getRun(ctx, runID)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if detail.State == runStateSuspending {
		m.suspending = true
	}
	if detail.State != runStateProvisioning && m.leftProvisioning.IsZero() {
		m.leftProvisioning = time.Now()
	}
}

func (m *runStateMonitor) stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		<-m.done
	})
}

func (m *runStateMonitor) firstLeftProvisioning() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leftProvisioning
}

func (m *runStateMonitor) sawSuspending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspending
}
