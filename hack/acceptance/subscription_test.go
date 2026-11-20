//go:build acceptance

package acceptance

// Substrate demonstration scenario and pass criteria: README "实现顺序" step 4
// and the seven pass criteria (the seventh is the 2026-11-20 supplement).
//
// The control plane runs on the development host with --provider substrate and
// --suspend, connected to connectorhub; one extsim process serves every
// subtest. Each subtest creates a fresh run instance. The pass verdict comes
// only from the criteria functions in criteria_test.go: collectCriteria never
// sets DryRun or SuspensionDisabled, so every criterion checks the suspension
// records the run produced.
//
// The subtests run in order and stop at the first failure; RUN=<name> runs one
// of them through the Makefile. The seventh subtest, ActorCrash, covers the
// criterion added after the original six; it makes the actor run a process
// that allocates past its memory limit.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

const (
	// The addresses the scenario binds. The first two are the development
	// host's NIC address, which the actors reach through the Services; the
	// webhook receiver and extsim's control interface are loopback only.
	subscriptionGRPCAddr      = "10.12.72.11:9090"
	subscriptionConnectorAddr = "10.12.72.11:9091"
	subscriptionWebhookAddr   = "127.0.0.1:9092"
	subscriptionExtsimListen  = "0.0.0.0:9100"
	subscriptionExtsimControl = "127.0.0.1:9101"

	// subscriptionExtsimLocalURL is what connectorhub's status query uses; the
	// control plane runs on the same host as extsim.
	subscriptionExtsimLocalURL = "http://127.0.0.1:9100"
	// subscriptionExtsimEgress is the actor-side extsim address.
	subscriptionExtsimEgress = "extsim.castworks-system.svc.cluster.local:9100"

	subscriptionNamespace = "castworks-system"
	subscriptionProject   = "demo"

	// subscriptionJobDuration is extsim's default --job-duration for this
	// scenario: the 120 s the plan requires so both callbacks arrive after the
	// actor is suspended.
	subscriptionJobDuration = 120 * time.Second
	// subscriptionBackgroundJobDuration is the plan the background-job case
	// appends: 90 s, longer than G_idle + P_scan + 10 s at a turn boundary.
	subscriptionBackgroundJobDuration = 90 * time.Second

	// subscriptionRunTimeout is shorter than the make target's -timeout 180m,
	// so a deadline fails the test and runs t.Cleanup instead of letting go
	// test panic.
	subscriptionRunTimeout     = 150 * time.Minute
	subscriptionSubtestTimeout = 25 * time.Minute
	subscriptionTurnTimeout    = 10 * time.Minute

	// subscriptionCrashCommand allocates 64 MiB buffers without bound. The
	// actor template limits memory to 2 GiB, so the sandbox exceeds its limit.
	subscriptionCrashCommand = `node -e 'const a=[]; for(;;) a.push(Buffer.alloc(64*1024*1024, 1));'`

	subscriptionCrashPollInterval = 500 * time.Millisecond
)

var subscriptionJobIDPattern = regexp.MustCompile(`job-[a-z2-7]{12}`)

// TestSubscriptionScenario runs the six planned scenarios plus the actor-crash
// scenario. A failing subtest stops the ones after it.
func TestSubscriptionScenario(t *testing.T) {
	ctx := testContext(t, subscriptionRunTimeout)

	requireEnv(t, envDatabaseURL)
	credentialFile := requireEnv(t, envCredentialFile)
	actorImage := requireEnv(t, envActorImage)
	if !actorImageRef.MatchString(actorImage) {
		t.Fatalf("acceptance: %s is %q, want name@sha256:<64 hex digits>", envActorImage, actorImage)
	}
	actorCommit := requireEnv(t, suspendActorCommitEnv)
	// Only check that the credential file exists; never open it.
	if _, err := os.Stat(credentialFile); err != nil {
		t.Fatalf("acceptance: credential file %s is not available: %v", credentialFile, err)
	}
	kubeContext := optionalEnv(envKubeContext)

	// Every precondition is checked before any process starts.
	checkSubscriptionPreconditions(t, ctx, kubeContext)
	repoHead := subscriptionRequireCleanRepo(t)
	// The measurement records the resolved SHA, not the value as supplied.
	actorCommit = subscriptionRequireActorCommitAncestor(t, actorCommit, repoHead)

	pool := pgtest.NewPool(t)
	h := newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "substrate",
	})
	h.buildBinaries()

	signingKey := h.generateSigningKey()
	configPath := h.writeConnectorhubConfig("", signingKey, subscriptionExtsimLocalURL)

	s := &subscriptionScenario{
		h:              h,
		ctx:            ctx,
		credentialFile: credentialFile,
		actorImage:     actorImage,
		actorCommit:    actorCommit,
		repoHead:       repoHead,
		controlArgs:    subscriptionControlPlaneArgs(credentialFile, actorImage, configPath, kubeContext),
	}
	if err := os.WriteFile(filepath.Join(h.workDir, "repo-head"),
		[]byte(repoHead+"\n"), 0o644); err != nil {
		t.Fatalf("acceptance: write repo-head: %v", err)
	}

	h.startControlPlane(ctx, s.controlArgs...)
	h.startExtsim(ctx, extsimOptions{
		Listen:        subscriptionExtsimListen,
		ControlListen: subscriptionExtsimControl,
		JobDuration:   subscriptionJobDuration,
	})

	t.Cleanup(func() {
		suspendWriteCombined(t, s.measurements)
	})

	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"Demo", s.runDemo},
		{"CallbackFirst", s.runCallbackFirst},
		{"CallbackDropped", s.runCallbackDropped},
		{"DuplicateDelivery", s.runDuplicateDelivery},
		{"RestartWhileSuspended", s.runRestartWhileSuspended},
		{"BackgroundJob", s.runBackgroundJob},
		{"ActorCrash", s.runActorCrash},
	}
	for _, step := range steps {
		if !t.Run(step.name, func(st *testing.T) {
			step.run(st)
		}) {
			t.Logf("acceptance: subtest %s failed; not running the subtests after it", step.name)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Preconditions

// checkSubscriptionPreconditions verifies, before any process starts, that
// the development host and the cluster are ready.
func checkSubscriptionPreconditions(t *testing.T, ctx context.Context, kubeContext string) {
	t.Helper()

	for _, addr := range []string{
		subscriptionGRPCAddr,
		subscriptionConnectorAddr,
		subscriptionWebhookAddr,
		subscriptionExtsimListen,
		subscriptionExtsimControl,
	} {
		if err := requireFreeAddr(addr); err != nil {
			t.Fatalf("acceptance: %s cannot be bound: %v; "+
				"another acceptance run or a connectorhub integration test may be using it", addr, err)
		}
	}

	checkSubscriptionService(t, ctx, kubeContext, "extsim", 9100)
	checkSubscriptionEndpointSlice(t, ctx, kubeContext, "extsim")
	checkSubscriptionService(t, ctx, kubeContext, "castworks-dev", 9091)
	checkNoLedgerActors(t, ctx, kubeContext)
	checkIdleSubstrateWorker(t, ctx, kubeContext)
}

// subscriptionKubectl runs plain kubectl (the ate plugin is only for actors and
// workers) and returns stdout. A failure includes stderr.
func subscriptionKubectl(ctx context.Context, kubeContext string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+2)
	if kubeContext != "" {
		full = append(full, "--context", kubeContext)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type subscriptionServiceDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Ports []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"ports"`
	} `json:"spec"`
}

// checkSubscriptionService requires a Service in the namespace to expose port.
func checkSubscriptionService(t *testing.T, ctx context.Context, kubeContext, name string, port int) {
	t.Helper()

	out, err := subscriptionKubectl(ctx, kubeContext, "get", "service", name, "-n", subscriptionNamespace, "-o", "json")
	if err != nil {
		t.Fatalf("acceptance: Service %s/%s is missing: %v; run deploy/dev/setup.sh first", subscriptionNamespace, name, err)
	}
	var service subscriptionServiceDoc
	if err := json.Unmarshal(out, &service); err != nil {
		t.Fatalf("acceptance: decode Service %s/%s: %v", subscriptionNamespace, name, err)
	}
	for _, p := range service.Spec.Ports {
		if p.Port == port {
			return
		}
	}
	t.Fatalf("acceptance: Service %s/%s has no port %d: %+v",
		subscriptionNamespace, name, port, service.Spec.Ports)
}

type subscriptionEndpointSliceList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Endpoints []struct {
			Addresses []string `json:"addresses"`
		} `json:"endpoints"`
	} `json:"items"`
}

// checkSubscriptionEndpointSlice requires at least one EndpointSlice for the
// Service with an address.
func checkSubscriptionEndpointSlice(t *testing.T, ctx context.Context, kubeContext, name string) {
	t.Helper()

	out, err := subscriptionKubectl(ctx, kubeContext, "get", "endpointslice", "-n", subscriptionNamespace,
		"-l", "kubernetes.io/service-name="+name, "-o", "json")
	if err != nil {
		t.Fatalf("acceptance: list EndpointSlices of Service %s: %v", name, err)
	}
	var list subscriptionEndpointSliceList
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("acceptance: decode EndpointSlices of Service %s: %v", name, err)
	}
	for _, item := range list.Items {
		for _, endpoint := range item.Endpoints {
			for _, addr := range endpoint.Addresses {
				if addr != "" {
					return
				}
			}
		}
	}
	t.Fatalf("acceptance: Service %s/%s has no EndpointSlice with an address; run deploy/dev/setup.sh first",
		subscriptionNamespace, name)
}

// subscriptionRequireActorCommitAncestor resolves the image's source commit
// to a full SHA, refuses to start when that commit is not in the tested
// repository's history, and returns the resolved SHA. Resolving first matters:
// a value such as HEAD or a branch name would otherwise compare the wrong
// thing, or accept a commit that is not an ancestor at all.
func subscriptionRequireActorCommitAncestor(t *testing.T, actorCommit, repoHead string) string {
	t.Helper()
	resolved, err := subscriptionCheckCommitAncestor(repoRoot(t), actorCommit, repoHead)
	if err != nil {
		t.Fatalf("acceptance: %s %q: %v", suspendActorCommitEnv, actorCommit, err)
	}
	return resolved
}

// subscriptionCheckCommitAncestor resolves revision to a commit and reports
// whether that commit is an ancestor of head. A revision git does not resolve
// to a commit is rejected before the ancestry check.
func subscriptionCheckCommitAncestor(root, revision, head string) (string, error) {
	resolved, err := subscriptionResolveCommit(root, revision)
	if err != nil {
		return "", err
	}
	if err := subscriptionCheckAncestor(root, resolved, head); err != nil {
		return "", err
	}
	return resolved, nil
}

// subscriptionCheckAncestor reports whether candidate is an ancestor of head.
func subscriptionCheckAncestor(root, candidate, head string) error {
	out, err := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", candidate, head).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s is not an ancestor of %s: %v\n%s", candidate, head, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// subscriptionResolveCommit resolves revision to its full 40-character commit
// SHA. It accepts a commit and a ref that points straight at one, such as HEAD,
// a branch or a lightweight tag. An annotated tag resolves to a tag object and
// is rejected, as is any other non-commit object, because the actor image's
// label must name a commit, not something git has to peel first.
func subscriptionResolveCommit(root, revision string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", revision).Output()
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", revision, err)
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 {
		return "", fmt.Errorf("%q resolved to %q, want a 40-character object SHA", revision, sha)
	}
	kind, err := exec.Command("git", "-C", root, "cat-file", "-t", sha).Output()
	if err != nil {
		return "", fmt.Errorf("read the object type of %q (%s): %w", revision, sha, err)
	}
	if got := strings.TrimSpace(string(kind)); got != "commit" {
		return "", fmt.Errorf("%q resolves to a %s object (%s), not a commit", revision, got, sha)
	}
	return sha, nil
}

// subscriptionControlPlaneArgs returns the v0.1.0 suspend/resume acceptance
// parameters plus connectorhub, the connector endpoint and the extsim egress.
func subscriptionControlPlaneArgs(credentialFile, actorImage, configPath, kubeContext string) []string {
	args := []string{
		"--provider", "substrate",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", subscriptionGRPCAddr,
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialFile,
		"--substrate-image", actorImage,
		"--suspend",
		"--idle-grace", suspendIdleGrace.String(),
		"--scan-period", suspendScanPeriod.String(),
		"--reconnect-timeout", criteriaReconnectTimeout.String(),
		"--connectorhub-config", configPath,
		"--connector-addr", subscriptionConnectorAddr,
		"--substrate-egress-http", subscriptionExtsimEgress,
		"--shutdown-timeout", "10s",
	}
	if kubeContext != "" {
		args = append(args, "--substrate-context", kubeContext)
	}
	return args
}

// ---------------------------------------------------------------------------
// Scenario fixture

type subscriptionScenario struct {
	h              *harness
	ctx            context.Context
	credentialFile string
	actorImage     string
	actorCommit    string
	repoHead       string
	controlArgs    []string

	mu           sync.Mutex
	measurements []suspendScenarioMeasurement
}

// subscriptionRun is one created run instance and its actor.
type subscriptionRun struct {
	id        string
	actor     string
	startedAt time.Time
	ended     bool
}

func (s *subscriptionScenario) subtestContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(s.ctx, subscriptionSubtestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// enterSubtest points the shared harness at the running subtest, so a fatal
// failure in a harness helper marks that subtest rather than the parent. The
// harness was created with the parent's *testing.T, and its cleanup uses the
// parent again. Subtests run sequentially.
func (s *subscriptionScenario) enterSubtest(t *testing.T) {
	previous := s.h.t
	s.h.t = t
	t.Cleanup(func() { s.h.t = previous })
}

// copyControlPlaneLogs saves the current control plane logs before
// startControlPlane truncates them for a restart.
func (h *harness) copyControlPlaneLogs(label string) {
	for _, name := range []string{"controlplane.stdout.log", "controlplane.stderr.log"} {
		data, err := os.ReadFile(filepath.Join(h.workDir, name))
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(h.workDir, label+"-"+name), data, 0o644)
	}
}

// clearPlans discards plans a previous subtest left unused. Every subtest
// starts from an empty plan queue.
func (s *subscriptionScenario) clearPlans(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := s.h.extsimControlClient().clearPlans(ctx); err != nil {
		t.Fatalf("acceptance: clear extsim plans: %v", err)
	}
}

// appendPlan queues one plan with an explicit duration, including 0. The
// harness's extsimPlan uses omitempty, which cannot express "duration_ms: 0".
func (s *subscriptionScenario) appendPlan(t *testing.T, ctx context.Context, duration *time.Duration, callback string) {
	t.Helper()
	body := map[string]any{}
	if duration != nil {
		body["duration_ms"] = duration.Milliseconds()
	}
	if callback != "" {
		body["callback"] = callback
	}
	if err := s.h.extsimControlClient().do(ctx, http.MethodPost, "/control/plans", body, nil); err != nil {
		t.Fatalf("acceptance: append extsim plan %v: %v", body, err)
	}
}

// createRunHTTP creates a run through the control plane API and registers a
// cleanup that removes whatever the subtest leaves behind.
func (s *subscriptionScenario) createRunHTTP(t *testing.T, ctx context.Context, prompt string) *subscriptionRun {
	t.Helper()
	r := &subscriptionRun{
		id:        s.h.createRun(ctx, prompt),
		startedAt: time.Now().UTC(),
	}
	r.actor = sandboxprovider.ActorName(r.id)
	t.Cleanup(func() { s.endRunBestEffort(t, r) })
	return r
}

// createRunCLI creates a run through the castworks command line, as the Demo
// scenario requires.
func (s *subscriptionScenario) createRunCLI(t *testing.T, ctx context.Context, prompt string) *subscriptionRun {
	t.Helper()
	r := &subscriptionRun{
		id:        s.h.suspendCreateRunCLI(ctx, prompt),
		startedAt: time.Now().UTC(),
	}
	r.actor = sandboxprovider.ActorName(r.id)
	t.Cleanup(func() { s.endRunBestEffort(t, r) })
	return r
}

// endRun ends the run, waits for the actor to disappear, and records how long
// the subtest took. The normal path uses the castworks command line.
func (s *subscriptionScenario) endRun(t *testing.T, ctx context.Context, r *subscriptionRun) {
	t.Helper()
	if r.ended {
		return
	}
	s.h.saveActorState(ctx, r.actor)
	if s.h.control == nil || s.h.control.exited() {
		t.Fatalf("acceptance: the control plane is not running; cannot end run %s", r.id)
	}
	if _, err := s.runCLI(ctx, "run", "end", r.id); err != nil {
		t.Fatalf("acceptance: end run %s: %v", r.id, err)
	}
	if err := waitActorGone(ctx, s.h.kubeCtx, r.actor, actorGoneTimeout); err != nil {
		t.Fatalf("acceptance: actor %s was not removed after End: %v", r.actor, err)
	}
	r.ended = true
	if s.h.runID == r.id {
		s.h.runID = ""
	}
	t.Logf("acceptance: run %s finished, actor %s is gone, subtest elapsed %s",
		r.id, r.actor, time.Since(r.startedAt).Round(time.Second))
}

// endRunBestEffort is the cleanup path: it ends the run when the control plane
// is still up, then deletes the actor directly when it is not gone. It never
// calls t.Fatalf from cleanup.
func (s *subscriptionScenario) endRunBestEffort(t *testing.T, r *subscriptionRun) {
	if r == nil || r.ended {
		return
	}
	if s.h.control != nil && !s.h.control.exited() && s.h.controlPlaneHTTPAddr() != "" {
		endCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.h.endRun(endCtx, r.id); err != nil {
			var statusErr *httpStatusError
			if !errors.As(err, &statusErr) {
				t.Logf("cleanup: end run %s: %v", r.id, err)
			}
		}
		cancel()
	}
	goneCtx, cancel := context.WithTimeout(context.Background(), actorGoneTimeout)
	err := waitActorGone(goneCtx, s.h.kubeCtx, r.actor, actorGoneTimeout)
	cancel()
	if err != nil {
		delCtx, delCancel := context.WithTimeout(context.Background(), cleanupKubectlTimeout)
		_, delErr := kubectlAte(delCtx, s.h.kubeCtx, "delete", "actor", r.actor, "-a", ateSpace)
		delCancel()
		if delErr != nil {
			t.Logf("cleanup: delete actor %s: %v (wait error: %v)", r.actor, delErr, err)
		}
	}
	r.ended = true
	if s.h.runID == r.id {
		s.h.runID = ""
	}
}

// runCLI runs the castworks command line against the running control plane.
func (s *subscriptionScenario) runCLI(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--server", "http://" + s.h.controlPlaneHTTPAddr()}, args...)
	cmd := exec.CommandContext(ctx, filepath.Join(s.h.binDir, "castworks"), full...)
	cmd.Env = childEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("castworks %s: %w\nstdout:\n%s\nstderr:\n%s",
			strings.Join(full, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

// ---------------------------------------------------------------------------
// Measurements

// subscriptionMeasurement creates the measurement record for one subtest.
func (s *subscriptionScenario) newMeasurement(t *testing.T, name string, r *subscriptionRun) *suspendScenarioMeasurement {
	m := newSuspendScenario(name)
	m.RunID = r.id
	m.Actor = r.actor
	m.ActorImage = s.actorImage
	m.ActorImageCommit = s.actorCommit
	m.RepoHead = s.repoHead
	m.RepoCommit = s.repoHead
	m.addNote("tested repo HEAD: %s", s.repoHead)
	m.addNote("actor image: %s", s.actorImage)
	m.addNote("actor source commit: %s", s.actorCommit)
	// A verdict before finishMeasurement still leaves its evidence on disk.
	t.Cleanup(func() {
		if m.Written {
			return
		}
		m.finish(t, filepath.Join(s.h.workDir, sanitizeTestName(name)))
	})
	return m
}

// finishMeasurement stops the suspension watcher and writes the subtest's
// measurements.json under its own directory. The snapshot sizes and detection
// instants the watcher attached to m are preserved by setSuspensions.
func (s *subscriptionScenario) finishMeasurement(t *testing.T, ctx context.Context,
	name string, r *subscriptionRun, detail runDetail, watcher *subscriptionSuspensionWatcher,
) {
	t.Helper()
	if watcher != nil {
		watcher.stop()
		if err := watcher.Err(); err != nil {
			t.Logf("acceptance: suspension watcher: %v", err)
		}
	}
	m := watcher.measurement
	if m == nil {
		return
	}
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	m.setSuspensions(detail)
	// The watcher polls once a second; a subtest can end between the last
	// suspension's snapshot_ms appearing and the next poll. Measure any
	// suspension the watcher did not reach while the actor still exists, so a
	// final suspension awaiting End still carries its snapshot size.
	for i, record := range detail.Suspensions {
		if i >= len(m.Suspensions) || m.Suspensions[i].SnapshotSize != nil || m.Suspensions[i].SnapshotNote != "" {
			continue
		}
		if m.Suspensions[i].DetectedSuspendedAt == nil {
			m.markSuspensionDetected(i, time.Now().UTC(), subscriptionPreviousTurn(detail.Turns, record.StartedAt))
		}
		suspendMeasureAndAttachSnapshot(ctx, s.h, m, watcher.tracker, i, r.actor)
	}
	for i := range detail.Turns {
		m.addTurn(detail.Turns[i], i+1, false, nil, nil)
	}
	m.finish(t, filepath.Join(s.h.workDir, sanitizeTestName(name)))
	s.mu.Lock()
	s.measurements = append(s.measurements, *m)
	s.mu.Unlock()
}

// subscriptionSuspensionWatcher polls the run while it lives. It records each
// suspension as soon as snapshot_ms appears, measures that generation's
// snapshot size while the actor is still suspended, and notes when the test
// first saw the suspended state.
type subscriptionSuspensionWatcher struct {
	h           *harness
	runID       string
	actor       string
	measurement *suspendScenarioMeasurement
	tracker     *suspendSnapshotTracker

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	measured map[int]bool
	err      error
}

func startSubscriptionSuspensionWatcher(ctx context.Context, h *harness, runID, actor string,
	m *suspendScenarioMeasurement,
) *subscriptionSuspensionWatcher {
	ctx, cancel := context.WithCancel(ctx)
	w := &subscriptionSuspensionWatcher{
		h:           h,
		runID:       runID,
		actor:       actor,
		measurement: m,
		tracker:     newSuspendSnapshotTracker(),
		cancel:      cancel,
		done:        make(chan struct{}),
		measured:    make(map[int]bool),
	}
	go w.run(ctx)
	return w
}

func (w *subscriptionSuspensionWatcher) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(defaultBGJobsInterval)
	defer ticker.Stop()
	for {
		w.observe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *subscriptionSuspensionWatcher) observe(ctx context.Context) {
	detail, err := w.h.getRun(ctx, w.runID)
	if err != nil {
		w.setErr(err)
		return
	}
	w.setErr(nil)

	w.mu.Lock()
	defer w.mu.Unlock()
	for i, record := range detail.Suspensions {
		if record.SnapshotMS == nil || w.measured[i] {
			continue
		}
		w.measured[i] = true
		w.measurement.setSuspensions(detail)
		previous := subscriptionPreviousTurn(detail.Turns, record.StartedAt)
		w.measurement.markSuspensionDetected(i, time.Now().UTC(), previous)
		suspendMeasureAndAttachSnapshot(ctx, w.h, w.measurement, w.tracker, i, w.actor)
	}
}

func (w *subscriptionSuspensionWatcher) setErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *subscriptionSuspensionWatcher) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *subscriptionSuspensionWatcher) stop() {
	w.cancel()
	<-w.done
}

// subscriptionPreviousTurn returns the last turn that ended at or before at.
func subscriptionPreviousTurn(turns []turn, at time.Time) *turn {
	var latest *turn
	for i := range turns {
		if turns[i].EndedAt == nil || turns[i].EndedAt.After(at) {
			continue
		}
		if latest == nil || turns[i].EndedAt.After(*latest.EndedAt) {
			latest = &turns[i]
		}
	}
	return latest
}

// ---------------------------------------------------------------------------
// Shared waits

// waitTurnsAndBoundary waits for n ended turns and a boundary, with a shorter
// per-subtest deadline.
func (s *subscriptionScenario) waitTurnsAndBoundary(t *testing.T, ctx context.Context, runID string, n int) runDetail {
	t.Helper()
	turnCtx, cancel := context.WithTimeout(ctx, subscriptionTurnTimeout)
	defer cancel()
	return s.h.waitTurnBoundary(turnCtx, runID, n)
}

// waitRunActive waits until the run is active and its actor is running.
func (s *subscriptionScenario) waitRunActive(t *testing.T, ctx context.Context, r *subscriptionRun) {
	t.Helper()
	err := waitFor(ctx, fmt.Sprintf("run %s to be active", r.id), func() (bool, error) {
		detail, err := s.h.getRun(ctx, r.id)
		if err != nil {
			return false, err
		}
		if detail.State == criteriaStateFailed {
			return false, fmt.Errorf("run %s failed: %s", r.id, detail.FailureReason)
		}
		return detail.State == "active", nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	err = waitFor(ctx, fmt.Sprintf("actor %s to be running", r.actor), func() (bool, error) {
		state, err := subscriptionActorState(ctx, s.h, r.actor)
		if err != nil {
			return false, nil
		}
		return strings.Contains(state, "RUNNING"), nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
}

// waitActorState waits until the actor's state contains want.
func (s *subscriptionScenario) waitActorState(t *testing.T, ctx context.Context, actor, want string) {
	t.Helper()
	var last string
	err := waitFor(ctx, fmt.Sprintf("actor %s to be %s", actor, want), func() (bool, error) {
		state, err := subscriptionActorState(ctx, s.h, actor)
		if err != nil {
			if isActorMissing(err) {
				return false, fmt.Errorf("actor %s is missing", actor)
			}
			return false, nil
		}
		last = state
		return strings.Contains(state, want), nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v (last state %q)", err, last)
	}
}

// subscriptionActorState reads the actor's state through kubectl ate.
func subscriptionActorState(ctx context.Context, h *harness, actor string) (string, error) {
	out, err := kubectlAte(ctx, h.kubeCtx, "get", "actor", actor, "-a", ateSpace, "-o", "json")
	if err != nil {
		return "", err
	}
	var doc struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return "", fmt.Errorf("decode actor %s: %w", actor, err)
	}
	return doc.Status.State, nil
}

// subscriptionActorExists reports whether the crashed actor is still in the
// atespace.
func subscriptionActorExists(ctx context.Context, h *harness, actor string) (bool, error) {
	_, err := kubectlAte(ctx, h.kubeCtx, "get", "actor", actor, "-a", ateSpace, "-o", "json")
	switch {
	case err == nil:
		return true, nil
	case isActorMissing(err):
		return false, nil
	default:
		return false, err
	}
}

// subscriptionWaitSubmission waits until the run has registered a subscription
// and returns its job id.
func (s *subscriptionScenario) subscriptionWaitSubmission(t *testing.T, ctx context.Context, runID string) string {
	t.Helper()
	var jobID string
	err := waitFor(ctx, fmt.Sprintf("run %s to register a subscription", runID), func() (bool, error) {
		subs, err := s.h.readSubscriptions(ctx, runID)
		if err != nil {
			return false, err
		}
		if len(subs) == 0 {
			return false, nil
		}
		jobID = criteriaJobID(subs[0])
		return jobID != "", nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	return jobID
}

// subscriptionWaitSuspended waits until the run has a completed suspension
// record and is suspended.
func (s *subscriptionScenario) subscriptionWaitSuspended(t *testing.T, ctx context.Context, runID string, want int) runDetail {
	t.Helper()
	var detail runDetail
	err := waitFor(ctx, fmt.Sprintf("run %s to reach %d suspension(s) and be suspended", runID, want), func() (bool, error) {
		current, err := s.h.getRun(ctx, runID)
		if err != nil {
			return false, err
		}
		if current.State == criteriaStateFailed {
			return false, fmt.Errorf("run %s failed: %s", runID, current.FailureReason)
		}
		detail = current
		if current.State != criteriaStateSuspended || len(current.Suspensions) < want {
			return false, nil
		}
		latest := current.Suspensions[len(current.Suspensions)-1]
		return latest.SnapshotMS != nil, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	return detail
}

// subscriptionJobIDFromOutput returns the last job id the report mentions.
func subscriptionJobIDFromOutput(output string) string {
	matches := subscriptionJobIDPattern.FindAllString(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// ---------------------------------------------------------------------------
// Subtests

// runDemo covers criterion 1: two round trips, each waiting through a
// suspension, the first submission failing and the second succeeding.
func (s *subscriptionScenario) runDemo(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)

	marker := "castworks-sub-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	r := s.createRunCLI(t, ctx, localSubscriptionPrompt(marker))
	m := s.newMeasurement(t, "Demo", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	detail := s.h.waitLocalSubscriptionReport(ctx, r.id)
	for i, tv := range detail.Turns {
		t.Logf("acceptance: demo turn %d: is_error=%t output=%s", i+1, tv.IsError, subscriptionExcerpt(tv.Output, 200))
	}

	c := s.h.collectCriteria(t, ctx, r.id)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	c.Expect.InitialMarker = marker
	recordCriteriaRecords(m, c, "")
	m.addCriteria("initial_marker", marker)
	m.addCriteria("turns", turnRecordEvidenceOf(detail.Turns))
	m.addCriteria("suspensions", suspensionRecordEvidenceOf(detail.Suspensions))
	assertJudge(t, "round trip", judgeRoundTrip(c))

	s.finishMeasurement(t, ctx, "Demo", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runCallbackFirst covers criterion 2: the callback arrives before the
// subscription is registered.
func (s *subscriptionScenario) runCallbackFirst(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)
	zero := time.Duration(0)
	s.appendPlan(t, ctx, &zero, "deliver")

	jobs := s.h.extsimJobs()
	job, err := jobs.submitJob(ctx, subscriptionProject, `{"owner":"castworks"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}
	ctl := s.h.extsimControlClient()
	err = waitFor(ctx, "extsim to deliver the job's callback", func() (bool, error) {
		attempts, err := ctl.deliveries(ctx, job.ID)
		if err != nil {
			return false, err
		}
		for _, attempt := range attempts {
			if criteriaIsSuccess(attempt.StatusCode) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	err = waitFor(ctx, "the connector event log to record the job", func() (bool, error) {
		events, err := s.h.readConnectorEvents(ctx)
		if err != nil {
			return false, err
		}
		return len(criteriaEventsForJob(events, job.ID)) > 0, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}

	r := s.createRunHTTP(t, ctx, subscriptionPrecreatedPrompt(job.ID))
	m := s.newMeasurement(t, "CallbackFirst", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	detail := s.waitTurnsAndBoundary(t, ctx, r.id, 2)

	c := s.h.collectCriteria(t, ctx, r.id, job.ID)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	c.Expect.JobID = job.ID
	recordCriteriaRecords(m, c, job.ID)
	callbackFirst := map[string]any{"source": callbackCompletionSource(c)}
	used := criteriaCallbackFirstEvidence(c)
	if used.Subscription != nil {
		sub := *used.Subscription
		callbackFirst["created_at"] = sub.CreatedAt
		callbackFirst["since_cursor"] = sub.SinceCursor
		callbackFirst["completed_at"] = sub.CompletedAt
		callbackFirst["status"] = sub.Status
	}
	// Record the event the judge matched, not the first event in the log.
	if used.Event != nil {
		callbackFirst["event_matched"] = true
		callbackFirst["received_at"] = used.Event.ReceivedAt
		callbackFirst["append_seq"] = used.Event.AppendSeq
		callbackFirst["delivery_id"] = used.Event.DeliveryID
		callbackFirst["event_type"] = used.Event.EventType
	} else {
		callbackFirst["event_matched"] = false
		m.addNote("callback_first: no connector event for job %s both arrived before the subscription was created and has append_seq below since_cursor", job.ID)
	}
	m.addCriteria("callback_first", callbackFirst)
	assertJudge(t, "callback first", judgeCallbackFirst(c))

	s.finishMeasurement(t, ctx, "CallbackFirst", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runCallbackDropped covers criterion 3: the job finished but its callback was
// dropped; registration's status query produces the completion event.
func (s *subscriptionScenario) runCallbackDropped(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)
	zero := time.Duration(0)
	s.appendPlan(t, ctx, &zero, "drop")

	jobs := s.h.extsimJobs()
	job, err := jobs.submitJob(ctx, subscriptionProject, `{"owner":"castworks"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}
	err = waitFor(ctx, "the job to complete", func() (bool, error) {
		current, err := jobs.getJob(ctx, job.ID)
		if err != nil {
			return false, err
		}
		return current.Status == "completed", nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	attempts, err := s.h.extsimControlClient().deliveries(ctx, job.ID)
	if err != nil {
		t.Fatalf("acceptance: read deliveries: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("acceptance: the drop plan produced %d callback attempts for job %s, want none", len(attempts), job.ID)
	}

	r := s.createRunHTTP(t, ctx, subscriptionPrecreatedPrompt(job.ID))
	m := s.newMeasurement(t, "CallbackDropped", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	detail := s.waitTurnsAndBoundary(t, ctx, r.id, 2)

	c := s.h.collectCriteria(t, ctx, r.id, job.ID)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	c.Expect.JobID = job.ID
	recordCriteriaRecords(m, c, job.ID)
	m.addCriteria("callback_dropped", map[string]any{
		"source":               callbackCompletionSource(c),
		"deliveries":           extsimDeliveryEvidenceOf(c.ExtsimDeliveries[job.ID]),
		"connector_event_rows": len(criteriaEventsForJob(c.ConnectorEvents, job.ID)),
	})
	assertJudge(t, "callback dropped", judgeCallbackDropped(c))

	s.finishMeasurement(t, ctx, "CallbackDropped", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runDuplicateDelivery covers criterion 4: two callbacks with different
// delivery identifiers, one inbox row, and a same-identifier replay answered
// with 200.
func (s *subscriptionScenario) runDuplicateDelivery(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)
	duration := subscriptionJobDuration
	s.appendPlan(t, ctx, &duration, "duplicate")

	jobs := s.h.extsimJobs()
	job, err := jobs.submitJob(ctx, subscriptionProject, `{"owner":"castworks"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}

	r := s.createRunHTTP(t, ctx, subscriptionPrecreatedPrompt(job.ID))
	m := s.newMeasurement(t, "DuplicateDelivery", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	// Turn 1 subscribes and ends; the run suspends before the callbacks
	// arrive. Turn 2 processes the completion.
	s.waitTurnsAndBoundary(t, ctx, r.id, 2)

	var events []connectorEventRecord
	err = waitFor(ctx, "the duplicate callbacks to reach the connector event log", func() (bool, error) {
		all, err := s.h.readConnectorEvents(ctx)
		if err != nil {
			return false, err
		}
		events = criteriaEventsForJob(all, job.ID)
		return len(events) == 2, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}

	status, err := s.h.replayWebhook(ctx, events[0].DeliveryID, events[0].Payload)
	if err != nil {
		t.Fatalf("acceptance: replay the webhook: %v", err)
	}

	observationStarted := time.Now().UTC()
	select {
	case <-ctx.Done():
		t.Fatalf("acceptance: duplicate observation window: %v", ctx.Err())
	case <-time.After(criteriaDuplicateWindow + time.Second):
	}
	observationEnded := time.Now().UTC()

	// The run may have suspended again while the observation window ran; a
	// turn boundary is not required here. The two processed turns were already
	// observed above, and the new suspension belongs in the measurement.
	detail, err := s.h.getRun(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: read run %s: %v", r.id, err)
	}
	c := s.h.collectCriteria(t, ctx, r.id, job.ID)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	turns, err := s.h.readTurns(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: read turns: %v", err)
	}
	c.Turns = turns
	c.Expect.JobID = job.ID
	c.Expect.ReplayStatus = status
	c.Times.ObservationStartedAt = observationStarted
	c.Times.ObservationEndedAt = observationEnded
	recordCriteriaRecords(m, c, job.ID)
	var successIDs []string
	for _, delivery := range c.ExtsimDeliveries[job.ID] {
		if criteriaIsSuccess(delivery.StatusCode) {
			successIDs = append(successIDs, delivery.DeliveryID)
		}
	}
	// Record every turn the judge examined for the observation window,
	// including its delivery identifier, so the turn evidence is traceable.
	windowTurns := criteriaTurnsInWindow(c.Turns, observationStarted, observationEnded)
	turnsInWindow := make([]map[string]any, 0, len(windowTurns))
	for _, observed := range windowTurns {
		evidence := map[string]any{
			"turn_id":     observed.TurnID,
			"started_at":  observed.StartedAt,
			"delivery_id": observed.DeliveryID,
		}
		if observed.EndedAt != nil {
			evidence["ended_at"] = observed.EndedAt.UTC()
		} else {
			evidence["ended_at"] = nil
		}
		turnsInWindow = append(turnsInWindow, evidence)
	}
	m.addCriteria("duplicate_delivery", map[string]any{
		"success_delivery_ids":        successIDs,
		"replay_status":               status,
		"connector_event_rows":        len(criteriaEventsForJob(c.ConnectorEvents, job.ID)),
		"completion_delivery_rows":    len(c.CompletionDeliveries),
		"inbox_rows":                  len(c.Inbox),
		"observation_started_at":      observationStarted,
		"observation_ended_at":        observationEnded,
		"observation_window_ms":       observationEnded.Sub(observationStarted).Milliseconds(),
		"turn_count":                  len(c.Turns),
		"turns_in_observation_window": turnsInWindow,
	})
	assertJudge(t, "duplicate delivery", judgeDuplicateDelivery(c))

	s.finishMeasurement(t, ctx, "DuplicateDelivery", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runRestartWhileSuspended covers criterion 5: the control plane restarts
// while the run is suspended; extsim retries the callback after the restart
// and the run resumes to process it.
func (s *subscriptionScenario) runRestartWhileSuspended(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)

	marker := "castworks-restart-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	r := s.createRunHTTP(t, ctx, subscriptionRestartPrompt(marker))
	m := s.newMeasurement(t, "RestartWhileSuspended", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	jobID := s.subscriptionWaitSubmission(t, ctx, r.id)
	s.subscriptionWaitSuspended(t, ctx, r.id, 1)
	s.waitActorState(t, ctx, r.actor, "SUSPENDED")

	runAtStop, err := s.h.readRun(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: read run before stop: %v", err)
	}

	suspendStopControlPlane(t, s.h, s.h.control)
	stoppedAt := time.Now().UTC()
	s.waitActorState(t, ctx, r.actor, "SUSPENDED")

	jobs := s.h.extsimJobs()
	err = waitFor(ctx, "the job to complete while the control plane is down", func() (bool, error) {
		current, err := jobs.getJob(ctx, jobID)
		if err != nil {
			return false, err
		}
		return current.Status == "completed", nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	ctl := s.h.extsimControlClient()
	err = waitFor(ctx, "two failed callback attempts while the control plane is down", func() (bool, error) {
		attempts, err := ctl.deliveries(ctx, jobID)
		if err != nil {
			return false, err
		}
		failed := 0
		for _, attempt := range attempts {
			if !criteriaIsSuccess(attempt.StatusCode) {
				failed++
			}
		}
		return failed >= 2, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}

	// Keep the first process's logs as evidence; startControlPlane truncates
	// the files it writes.
	s.h.copyControlPlaneLogs("controlplane-before-restart")
	restartedAt := time.Now().UTC()
	s.h.startControlPlane(ctx, s.controlArgs...)
	listeningAt := s.h.webhookListeningTimeFromLog()

	detail := s.waitTurnsAndBoundary(t, ctx, r.id, 2)

	c := s.h.collectCriteria(t, ctx, r.id, jobID)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	c.Expect.JobID = jobID
	c.Times.ControlPlaneStoppedAt = stoppedAt
	c.Times.ControlPlaneRestartedAt = restartedAt
	c.Times.WebhookListeningAt = listeningAt
	c.Times.RunStateAtStop = runAtStop.State
	recordCriteriaRecords(m, c, jobID)
	restart := map[string]any{
		"stopped_at":           stoppedAt,
		"restarted_at":         restartedAt,
		"webhook_listening_at": listeningAt,
		"run_state_at_stop":    runAtStop.State,
	}
	if fact, ok := criteriaJobFactFor(c.ConnectorEvents, jobID); ok {
		restart["job_completed_at"] = fact.UpdatedAt
	}
	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) > 0 {
		restart["inbox_created_at"] = rows[0].CreatedAt
		if resumed, mismatch := criteriaTurnProcessingEvent(c.Turns, rows[0].CreatedAt, c.Expect.TurnDeadline); mismatch == "" {
			restart["post_restart_turn_started_at"] = resumed.StartedAt
		}
	}
	m.addCriteria("restart", restart)
	m.addCriteria("turns", turnRecordEvidenceOf(detail.Turns))
	m.addCriteria("suspensions", suspensionRecordEvidenceOf(detail.Suspensions))
	assertJudge(t, "control plane restart", judgeRestartWhileSuspended(c))

	m.ControlPlaneRestarts = 1
	m.ControlPlaneRestartAt = &restartedAt
	m.addNote("control plane stopped at %s and restarted at %s", stoppedAt.Format(time.RFC3339Nano), restartedAt.Format(time.RFC3339Nano))
	m.addNote("job %s completed while the control plane was down; extsim retried after the restart", jobID)

	s.finishMeasurement(t, ctx, "RestartWhileSuspended", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runBackgroundJob covers criterion 6: a background job keeps the run from
// suspending; after it ends and a spontaneous turn appears, the run suspends
// only after the idle grace.
func (s *subscriptionScenario) runBackgroundJob(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)
	duration := subscriptionBackgroundJobDuration
	s.appendPlan(t, ctx, &duration, "deliver")

	marker := "castworks-bg-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	r := s.createRunHTTP(t, ctx, subscriptionBackgroundPrompt(marker))
	m := s.newMeasurement(t, "BackgroundJob", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	observer, err := s.h.startBGJobsObserver(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: start bg_jobs observer: %v", err)
	}
	t.Cleanup(observer.stop)

	// Turn 1 starts the background watch and ends; turn 2 is the spontaneous
	// turn after the job finishes.
	detail := s.waitTurnsAndBoundary(t, ctx, r.id, 2)

	err = waitFor(ctx, "bg_jobs to become non-empty and then empty", func() (bool, error) {
		changes := observer.Changes()
		seenNonEmpty := false
		for _, change := range changes {
			if len(change.Jobs) > 0 {
				seenNonEmpty = true
				continue
			}
			if seenNonEmpty {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("acceptance: %v; changes: %v", err, observer.Changes())
	}

	// The run suspends after the spontaneous turn ends and the idle grace
	// expires. Wait for that suspension so the criterion has a record to check.
	detail = s.subscriptionWaitSuspended(t, ctx, r.id, 1)
	observer.stop()
	if err := observer.Err(); err != nil {
		t.Fatalf("acceptance: bg_jobs observer stopped: %v", err)
	}

	reported := subscriptionJobIDFromOutput(subscriptionTurnsOutput(detail.Turns))
	if reported == "" {
		t.Fatalf("acceptance: the background-job report does not state a job id:\n%s",
			subscriptionTurnsOutput(detail.Turns))
	}
	jobs := s.h.extsimJobs()
	job, err := jobs.getJob(ctx, reported)
	if err != nil {
		t.Fatalf("acceptance: the reported job %s cannot be read: %v", reported, err)
	}
	if job.Status != "completed" {
		t.Fatalf("acceptance: the reported job %s has status %q, want completed", reported, job.Status)
	}
	t.Logf("acceptance: background job %s finished with %s", job.ID, job.Conclusion)

	c := s.h.collectCriteria(t, ctx, r.id, reported)
	suspendAttachSuspensionTimes(ctx, t, s.h, r.id, detail.Suspensions)
	c.Suspensions = detail.Suspensions
	// readTurns is the only source of the delivery identifier, which tells the
	// spontaneous turn apart from a delivered one.
	turns, err := s.h.readTurns(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: read turns: %v", err)
	}
	c.Turns = turns
	changes := observer.Changes()
	c.BGJobs = changes
	// Include the job id in the record so the judge reads its deliveries too.
	c.Expect.JobID = reported
	recordCriteriaRecords(m, c, reported)
	m.addCriteria("bg_jobs", changes)
	m.addCriteria("turns", turnRecordEvidenceOf(turns))
	m.addCriteria("suspensions", suspensionRecordEvidenceOf(detail.Suspensions))
	background := map[string]any{}
	used := criteriaBackgroundJobEvidence(c)
	if !used.EmptiedAt.IsZero() {
		background["emptied_at"] = used.EmptiedAt
	}
	if used.Spontaneous != nil {
		background["spontaneous_turn_id"] = used.Spontaneous.TurnID
		background["spontaneous_turn_delivery_id"] = used.Spontaneous.DeliveryID
		background["spontaneous_turn_started_at"] = used.Spontaneous.StartedAt
		background["spontaneous_turn_ended_at"] = used.Spontaneous.EndedAt
	}
	if used.Suspension != nil {
		background["suspension_started_at"] = used.Suspension.StartedAt
		background["suspension_number"] = criteriaSuspensionNumber(c.Suspensions, used.Suspension)
	}
	m.addCriteria("background_job", background)
	assertJudge(t, "background job", judgeBackgroundJob(c))

	s.finishMeasurement(t, ctx, "BackgroundJob", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// runActorCrash covers criterion 7: the actor crashes while the run is active.
// The test makes it run a process that allocates past the template's 2 GiB
// memory limit; it never lowers the template limits.
func (s *subscriptionScenario) runActorCrash(t *testing.T) {
	s.enterSubtest(t)
	ctx := s.subtestContext(t)
	s.clearPlans(t, ctx)

	r := s.createRunHTTP(t, ctx, subscriptionCrashPrompt())
	m := s.newMeasurement(t, "ActorCrash", r)
	watcher := startSubscriptionSuspensionWatcher(ctx, s.h, r.id, r.actor, m)
	t.Cleanup(watcher.stop)

	s.waitRunActive(t, ctx, r)

	crash := s.waitActorCrash(t, ctx, r)
	crashAt, crashState := crash.CrashedAt, crash.State
	if crash.LastRunningAt.IsZero() {
		t.Logf("acceptance: actor %s was never polled RUNNING; it was first observed in state %q at %s while run %s was active",
			r.actor, crashState, crashAt.Format(time.RFC3339Nano), r.id)
	} else {
		t.Logf("acceptance: actor %s last polled RUNNING at %s and first observed in state %q at %s; run %s was active",
			r.actor, crash.LastRunningAt.Format(time.RFC3339Nano), crashState,
			crashAt.Format(time.RFC3339Nano), r.id)
	}

	var record runRecord
	err := waitFor(ctx, fmt.Sprintf("run %s to turn failed after the actor crash", r.id), func() (bool, error) {
		current, err := s.h.readRun(ctx, r.id)
		if err != nil {
			return false, err
		}
		record = current
		return current.State == criteriaStateFailed, nil
	})
	if err != nil {
		// Report the actor state and the run state so a gVisor that only kills
		// the exceeding process is visible instead of looking like a timeout.
		state, stateErr := subscriptionActorState(ctx, s.h, r.actor)
		t.Fatalf("acceptance: %v; actor state %q (error %v), run state %q reason %q",
			err, state, stateErr, record.State, record.FailureReason)
	}
	runFailedAt := time.Now().UTC()
	if record.EndedAt != nil {
		runFailedAt = record.EndedAt.UTC()
	}

	exists, err := subscriptionActorExists(ctx, s.h, r.actor)
	if err != nil {
		t.Fatalf("acceptance: check actor %s: %v", r.actor, err)
	}

	detail, err := s.h.getRun(ctx, r.id)
	if err != nil {
		t.Fatalf("acceptance: read run %s: %v", r.id, err)
	}
	// The control plane logs the disconnect that starts T_reconnect. Without
	// that line the crash time is not observable, so fail instead of falling
	// back to the poll that first saw CRASHED. A run can log the control stream
	// closing more than once; the line that starts T_reconnect is the last one
	// at or before the first crashed poll plus 1 s.
	disconnect, disconnectLogged := subscriptionFindDisconnectLog(t, s.h, r.id, crashAt.Add(time.Second))
	if len(detail.Turns) == 0 {
		t.Fatalf("acceptance: run %s recorded no turns, so the trigger turn's start cannot bound the memory hog process", r.id)
	}
	// The first turn delivers subscriptionCrashPrompt; its start is a lower
	// bound on when the memory hog process started.
	processStartedAt := detail.Turns[0].StartedAt.UTC()
	if err := subscriptionCheckCrashTimeline(processStartedAt, disconnect.DisconnectedAt, crashAt, disconnectLogged); err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	c := s.h.collectCriteria(t, ctx, r.id)
	c.Suspensions = detail.Suspensions
	c.RunState = record.State
	c.RunFailureReason = record.FailureReason
	c.ActorExists = &exists
	c.ReconnectTimeout = criteriaReconnectTimeout
	c.ScanPeriod = criteriaScanPeriod
	c.Times.ActorCrashedAt = disconnect.DisconnectedAt
	c.Times.RunFailedAt = runFailedAt
	assertJudge(t, "actor crash", judgeActorCrash(c))

	m.addNote("crash command: %s", subscriptionCrashCommand)
	m.addNote("memory hog process start lower bound: the trigger turn started at %s", processStartedAt.Format(time.RFC3339Nano))
	m.addNote("actor state observed at the first crashed poll: %s", crashState)
	m.addNote("control plane logged the control stream disconnect at %s: %s",
		disconnect.DisconnectedAt.Format(time.RFC3339Nano), disconnect.Line)
	m.addNote("run turned failed at %s with reason %q (%.1fs after the control plane logged the disconnect)",
		runFailedAt.Format(time.RFC3339Nano), record.FailureReason, runFailedAt.Sub(disconnect.DisconnectedAt).Seconds())
	m.addNote("crashed actor still exists: %t", exists)
	// A zero time means the poll never saw the actor RUNNING; write the fact,
	// not a zero time.
	if crash.LastRunningAt.IsZero() {
		m.addNote("last_running_poll_at is absent: the actor was never polled in state RUNNING before the crash was observed")
	}
	m.addCriteria("actor_crash", subscriptionActorCrashEvidence(crash, disconnect, processStartedAt, record, runFailedAt, exists))

	s.finishMeasurement(t, ctx, "ActorCrash", r, detail, watcher)
	s.endRun(t, ctx, r)
}

// subscriptionCrashObservation is what the crash poll saw: the last poll that
// still found the actor RUNNING, the first poll that found it crashed or
// missing, and that state.
type subscriptionCrashObservation struct {
	LastRunningAt time.Time
	CrashedAt     time.Time
	State         string
}

// subscriptionDisconnectLog is the control plane's record of one run's
// outbound control stream closing: the instant it logged and the raw log line
// the instant came from.
type subscriptionDisconnectLog struct {
	DisconnectedAt time.Time
	Line           string
}

const subscriptionDisconnectLogMessage = "runcontroller: control stream closed"

// subscriptionCheckCrashTimeline checks that the observations of one crash
// agree. The trigger turn started at or before the control plane logged the
// disconnect, and the disconnect came at most 1 s after the first poll that saw
// the actor crashed. A missing or unparseable log line leaves no usable
// disconnect moment, so the parsed flag turns that into an error instead of
// comparing against a zero time.
func subscriptionCheckCrashTimeline(triggerTurnStartedAt, disconnectedAt, firstCrashedPollAt time.Time, disconnectLogParsed bool) error {
	if !disconnectLogParsed {
		return fmt.Errorf("the control plane log line recording the disconnect is missing or cannot be parsed, so the disconnect moment is not observable")
	}
	if triggerTurnStartedAt.After(disconnectedAt) {
		return fmt.Errorf("the trigger turn started at %s, after the control plane logged the disconnect at %s; the crash must follow the turn start",
			triggerTurnStartedAt.Format(time.RFC3339Nano), disconnectedAt.Format(time.RFC3339Nano))
	}
	if limit := firstCrashedPollAt.Add(time.Second); disconnectedAt.After(limit) {
		return fmt.Errorf("the control plane logged the disconnect at %s, after the first crashed poll at %s plus 1s (limit %s)",
			disconnectedAt.Format(time.RFC3339Nano), firstCrashedPollAt.Format(time.RFC3339Nano), limit.Format(time.RFC3339Nano))
	}
	return nil
}

// subscriptionFindDisconnectLog reads the control plane's log files and
// returns the last line for runID whose disconnected_at is not after notAfter.
// A run can log the control stream closing more than once, and only the line
// that starts T_reconnect, at or before the first crashed poll plus 1 s, is the
// crash moment. The control plane logs through slog's text handler on stderr;
// stdout is scanned too so the lookup survives a handler change. A run with no
// qualifying line returns ok=false: the disconnect moment is criterion input,
// never a guess.
func subscriptionFindDisconnectLog(t *testing.T, h *harness, runID string, notAfter time.Time) (subscriptionDisconnectLog, bool) {
	t.Helper()
	var last subscriptionDisconnectLog
	var found bool
	for _, name := range []string{"controlplane.stderr.log", "controlplane.stdout.log"} {
		data, err := os.ReadFile(filepath.Join(h.workDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, subscriptionDisconnectLogMessage) {
				continue
			}
			attrs := logAttributes(line)
			if attrs["run_id"] != runID {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, attrs["disconnected_at"])
			if err != nil {
				t.Fatalf("acceptance: control plane log line for run %s has disconnected_at %q, not RFC3339Nano: %v\n%s",
					runID, attrs["disconnected_at"], err, line)
			}
			if at = at.UTC(); at.After(notAfter) {
				continue
			}
			last = subscriptionDisconnectLog{DisconnectedAt: at, Line: line}
			found = true
		}
	}
	return last, found
}

// subscriptionActorCrashEvidence builds the raw values the crash criterion
// reads. ActorCrashedAt is the control plane's disconnected_at, not the first
// poll that saw CRASHED. last_running_poll_at is omitted when the poll never
// saw the actor RUNNING: a zero time would look like a real moment.
func subscriptionActorCrashEvidence(crash subscriptionCrashObservation, disconnect subscriptionDisconnectLog, processStartedAt time.Time, record runRecord, runFailedAt time.Time, exists bool) map[string]any {
	// trigger_turn_started_at is when the turn that delivered the crash prompt
	// started. It is a lower bound on when the memory hog process started, not
	// the process start itself.
	evidence := map[string]any{
		"first_crashed_poll_at":   crash.CrashedAt,
		"crash_state":             crash.State,
		"trigger_turn_started_at": processStartedAt,
		"disconnected_at":         disconnect.DisconnectedAt,
		"disconnect_log_line":     disconnect.Line,
		"run_state":               record.State,
		"run_failed_at":           runFailedAt,
		"run_ended_at":            record.EndedAt,
		"failure_reason":          record.FailureReason,
		"actor_exists":            exists,
	}
	if !crash.LastRunningAt.IsZero() {
		evidence["last_running_poll_at"] = crash.LastRunningAt
	}
	return evidence
}

// waitActorCrash waits until the actor is observed crashed or missing. An
// actor that suspends first means the crash was not produced while the run was
// active.
func (s *subscriptionScenario) waitActorCrash(t *testing.T, ctx context.Context, r *subscriptionRun) subscriptionCrashObservation {
	t.Helper()
	ticker := time.NewTicker(subscriptionCrashPollInterval)
	defer ticker.Stop()
	var observation subscriptionCrashObservation
	for {
		polledAt := time.Now().UTC()
		state, err := subscriptionActorState(ctx, s.h, r.actor)
		switch {
		case err != nil && isActorMissing(err):
			observation.CrashedAt = polledAt
			observation.State = "not found"
			return observation
		case err != nil:
			// A transient API error is not the crash observation.
		case strings.Contains(state, "CRASHED"):
			observation.CrashedAt = polledAt
			observation.State = state
			return observation
		case strings.Contains(state, "SUSPENDED"):
			t.Fatalf("acceptance: actor %s was %q before it crashed; the crash must happen while the run is active",
				r.actor, state)
		case strings.Contains(state, "RUNNING"):
			observation.LastRunningAt = polledAt
		}
		select {
		case <-ctx.Done():
			state, _ := subscriptionActorState(ctx, s.h, r.actor)
			t.Fatalf("acceptance: actor %s did not crash within the deadline; last state %q", r.actor, state)
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Prompts

// subscriptionPrecreatedPrompt tells the Agent to subscribe for a job the test
// has submitted itself. Whether the job has already completed or is still
// running, the completion event reaches the Agent as its next input.
func subscriptionPrecreatedPrompt(jobID string) string {
	return fmt.Sprintf(`Use the platform MCP tool %q with system "extsim" to confirm that job.completed is listed, then call %q with system "extsim", event "job.completed" and correlation {"job.id": %q}. The host already knows job %s. End the turn right after subscribing; the completion event will arrive as your next input. When it arrives, reply with the single line: outcome: <the job's terminal value>.`,
		localSubscriptionToolListEvents, localSubscriptionToolSubscribe, jobID, jobID)
}

// subscriptionRestartPrompt makes the Agent submit a passing job and
// subscribe, so the run can suspend while the job is still running.
func subscriptionRestartPrompt(marker string) string {
	return fmt.Sprintf(`Submit this JSON content to extsim with "extsim job submit" (write it to a file and pass --file):
{"owner":"castworks","note":%q}

After submitting, call the platform MCP tool %q with system "extsim", then call %q with system "extsim", event "job.completed" and correlation {"job.id": "<job id>"}. End the turn right after subscribing; the completion event will arrive as your next input. When it arrives, reply with the single line: outcome: <the job's terminal value>.`, marker, localSubscriptionToolListEvents, localSubscriptionToolSubscribe)
}

// subscriptionBackgroundPrompt makes the Agent submit a job, watch it in the
// background, and report when the spontaneous turn starts.
func subscriptionBackgroundPrompt(marker string) string {
	return fmt.Sprintf(`Submit this JSON content to extsim with "extsim job submit" (write it to a file and pass --file):
{"owner":"castworks","note":%q}

Then start "extsim job watch <job id>" as a background Bash task (use the run_in_background option). As soon as the background task is running, reply with the single word started and end the turn. Do not subscribe to the job and do not wait for it in the foreground. When the background task finishes, start a turn and report exactly these two lines:
job: <job id>
outcome: <the job's conclusion>`, marker)
}

// subscriptionCrashPrompt makes the Agent run the memory hog in the
// foreground, so the turn stays active while the actor exceeds its limit.
func subscriptionCrashPrompt() string {
	return fmt.Sprintf(`Run exactly this command in Bash in the foreground and wait for it to finish. Do not change it, do not run it in the background, and do not retry it if it is killed:

%s

When it exits, reply with the single word done.`, subscriptionCrashCommand)
}

// ---------------------------------------------------------------------------
// Helpers

// subscriptionTurnsOutput joins the outputs of the turns for parsing a report.
func subscriptionTurnsOutput(turns []turn) string {
	var b strings.Builder
	for i := range turns {
		b.WriteString(turns[i].Output)
		b.WriteString("\n")
	}
	return b.String()
}

// subscriptionExcerpt truncates a turn output for a log line.
func subscriptionExcerpt(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}
