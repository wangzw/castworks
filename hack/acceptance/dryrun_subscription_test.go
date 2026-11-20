//go:build acceptance

package acceptance

// TestDryRunSubscription drives the same orchestration as the local acceptance
// run with the real control plane (--provider local with connectorhub), the
// real extsim, a real warden and the scripted fake Claude Code. Local mode
// forces suspension off, so the run never suspends; the subtests call the
// same judge functions as the real tests and those functions skip exactly the
// suspension checks (criteriaCase.DryRun). It proves that the orchestration,
// the cleanup and the judges work on real records. It says nothing about the
// behavior of the real Claude Code.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

const (
	dryRunExtsimProject = "demo"
	dryRunCredential    = "acceptance-fake-token\n"
)

func TestDryRunSubscription(t *testing.T) {
	t.Run("Loop", testDryRunSubscriptionLoop)
	t.Run("CallbackFirst", testDryRunSubscriptionCallbackFirst)
	t.Run("CallbackDropped", testDryRunSubscriptionCallbackDropped)
	t.Run("DuplicateDelivery", testDryRunSubscriptionDuplicateDelivery)
	t.Run("RestartControlPlane", testDryRunSubscriptionRestart)
	t.Run("BackgroundJob", testDryRunSubscriptionBackgroundJob)
}

// ---------------------------------------------------------------------------
// Fixture

// dryRunFixtureOptions carries what a subtest needs before anything starts.
type dryRunFixtureOptions struct {
	// JobDuration is extsim's default --job-duration.
	JobDuration time.Duration
	// Plans are queued through the test control interface before the run.
	Plans []extsimPlan
	// Script is the fake Claude Code script copied into the warden's working
	// directory before the warden starts.
	Script string
	// FixedAddrs gives the control plane stable addresses, which the restart
	// subtest needs so the warden and extsim can reconnect.
	FixedAddrs bool
	// ShutdownTimeout is passed as --shutdown-timeout; the restart subtest
	// shortens it because a connected warden keeps gRPC graceful stop waiting.
	ShutdownTimeout string
}

type dryRunFixture struct {
	h           *harness
	claudeBin   string
	controlArgs []string
}

// startDryRunFixture starts an independent database, control plane and extsim
// for one subtest. The harness cleanup stops the processes and removes the
// generated key and configuration.
func startDryRunFixture(t *testing.T, ctx context.Context, opts dryRunFixtureOptions) *dryRunFixture {
	t.Helper()

	requireEnv(t, envDatabaseURL)
	pool := pgtest.NewPool(t)

	h := newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "local",
	})
	h.buildBinaries()
	claudeBin := h.buildFakeClaude()

	credentialPath := filepath.Join(h.workDir, "llm-credential")
	if err := os.WriteFile(credentialPath, []byte(dryRunCredential), 0o600); err != nil {
		t.Fatalf("acceptance: write fake credential: %v", err)
	}

	signingKey := h.generateSigningKey()
	configPath := h.writeConnectorhubConfig("", signingKey, "http://"+localExtsimJobsAddr)

	httpAddr, grpcAddr := "127.0.0.1:0", "127.0.0.1:0"
	connectorAddr, webhookAddr := "127.0.0.1:0", "127.0.0.1:0"
	if opts.FixedAddrs {
		httpAddr = freeLoopbackAddr(t)
		grpcAddr = freeLoopbackAddr(t)
		connectorAddr = freeLoopbackAddr(t)
		webhookAddr = freeLoopbackAddr(t)
	}
	shutdownTimeout := opts.ShutdownTimeout
	if shutdownTimeout == "" {
		shutdownTimeout = "10s"
	}

	args := []string{
		"--provider", "local",
		"--http-addr", httpAddr,
		"--grpc-addr", grpcAddr,
		"--connector-addr", connectorAddr,
		"--webhook-addr", webhookAddr,
		"--connectorhub-config", configPath,
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialPath,
		"--shutdown-timeout", shutdownTimeout,
	}
	h.startControlPlane(ctx, args...)
	h.startExtsim(ctx, extsimOptions{
		Listen:        localExtsimJobsAddr,
		ControlListen: "127.0.0.1:0",
		JobDuration:   opts.JobDuration,
	})
	ctl := h.extsimControlClient()
	// Each subtest gets its own extsim, so this only pins the starting state;
	// the Substrate run reuses one extsim and clears its plans between
	// scenarios.
	if err := ctl.clearPlans(ctx); err != nil {
		t.Fatalf("acceptance: clear extsim plans: %v", err)
	}
	for _, plan := range opts.Plans {
		if err := ctl.appendPlan(ctx, plan); err != nil {
			t.Fatalf("acceptance: queue extsim plan %+v: %v", plan, err)
		}
	}
	h.writeFakeClaudeScript(opts.Script)

	return &dryRunFixture{h: h, claudeBin: claudeBin, controlArgs: args}
}

// startRun creates the run and starts the warden against it.
func (f *dryRunFixture) startRun(ctx context.Context, prompt string) string {
	f.h.t.Helper()
	runID := f.h.createRun(ctx, prompt)
	f.h.startWardenLocal(runID, f.claudeBin)
	return runID
}

// finish ends the run and checks that the warden exits cleanly.
func (f *dryRunFixture) finish(ctx context.Context, runID string) {
	f.h.t.Helper()
	if err := f.h.endRun(ctx, runID); err != nil {
		f.h.t.Fatalf("acceptance: end run %s: %v", runID, err)
	}
	if err := f.h.waitChild(f.h.warden, 30*time.Second); err != nil {
		f.h.t.Fatalf("acceptance: warden exited with an error: %v", err)
	}
}

// freeLoopbackAddr reserves an ephemeral loopback port and releases it. It is
// used only where a restart has to find the same address again.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("acceptance: reserve a loopback address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("acceptance: release %s: %v", addr, err)
	}
	return addr
}

// buildFakeClaude builds the scripted Claude Code into the harness binary
// directory and returns its path.
func (h *harness) buildFakeClaude() string {
	h.t.Helper()
	path := filepath.Join(h.binDir, "fakeclaude")
	cmd := exec.Command("go", "build", "-o", path, "./hack/acceptance/fakeclaude")
	cmd.Dir = h.repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("acceptance: go build ./hack/acceptance/fakeclaude: %v\n%s", err, out)
	}
	return path
}

// writeFakeClaudeScript copies the script to fakeclaude.json in the
// directory warden runs Claude Code in (<home>/work).
func (h *harness) writeFakeClaudeScript(source string) {
	h.t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		h.t.Fatalf("acceptance: read fake Claude Code script %s: %v", source, err)
	}
	workDir := filepath.Join(h.workDir, "home", "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		h.t.Fatalf("acceptance: create Claude Code working directory: %v", err)
	}
	path := filepath.Join(workDir, "fakeclaude.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		h.t.Fatalf("acceptance: write fake Claude Code script %s: %v", path, err)
	}
}

// loggedTime matches the time attribute of a slog text line.
var loggedTime = regexp.MustCompile(`\btime=(\S+)`)

// webhookListeningTimeFromLog returns the moment the control plane logged its
// webhook listener. The observation time is only an upper bound; a retry that
// arrives between the log line and the next poll must still count as after
// the listener came up.
func (h *harness) webhookListeningTimeFromLog() time.Time {
	for _, path := range []string{
		filepath.Join(h.workDir, "controlplane.stderr.log"),
		filepath.Join(h.workDir, "controlplane.stdout.log"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "webhook listening") {
				continue
			}
			match := loggedTime.FindStringSubmatch(line)
			if len(match) != 2 {
				continue
			}
			if at, err := time.Parse(time.RFC3339Nano, match[1]); err == nil {
				return at
			}
		}
	}
	return h.webhookListeningAt
}

// ---------------------------------------------------------------------------
// Record collection

// collectDryRunCriteria reads the records every judge takes. The suspension
// records are read as well, but the judges skip them when DryRun is set.
func (h *harness) collectDryRunCriteria(t *testing.T, ctx context.Context, runID string, extraJobIDs ...string) criteriaCase {
	t.Helper()

	detail, err := h.getRun(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read run %s: %v", runID, err)
	}
	inbox, err := h.readInbox(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read inbox: %v", err)
	}
	subscriptions, err := h.readSubscriptions(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read subscriptions: %v", err)
	}
	events, err := h.readConnectorEvents(ctx)
	if err != nil {
		t.Fatalf("acceptance: read connector events: %v", err)
	}
	completions, err := h.readCompletionDeliveries(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read completion deliveries: %v", err)
	}

	jobIDs := make(map[string]struct{})
	for _, sub := range subscriptions {
		if jobID := criteriaJobID(sub); jobID != "" {
			jobIDs[jobID] = struct{}{}
		}
	}
	for _, jobID := range extraJobIDs {
		jobIDs[jobID] = struct{}{}
	}
	ctl := h.extsimControlClient()
	deliveries := make(map[string][]extsimDelivery, len(jobIDs))
	for jobID := range jobIDs {
		attempts, err := ctl.deliveries(ctx, jobID)
		if err != nil {
			t.Fatalf("acceptance: read extsim deliveries for %s: %v", jobID, err)
		}
		deliveries[jobID] = attempts
	}

	return criteriaCase{
		Turns:                detail.Turns,
		Suspensions:          detail.Suspensions,
		Inbox:                inbox,
		Subscriptions:        subscriptions,
		ConnectorEvents:      events,
		CompletionDeliveries: completions,
		ExtsimDeliveries:     deliveries,
		BGJobsResolution:     defaultBGJobsInterval,
		DryRun:               true,
	}
}

// assertJudge fails on the first mismatch a judge returns.
func assertJudge(t *testing.T, name, mismatch string) {
	t.Helper()
	if mismatch != "" {
		t.Fatalf("acceptance: %s criterion: %s", name, mismatch)
	}
}

// waitForDryRun polls check until it holds, failing the test on an error or a
// timeout.
func waitForDryRun(t *testing.T, ctx context.Context, description string, check func() (bool, error)) {
	t.Helper()
	if err := waitFor(ctx, description, check); err != nil {
		t.Fatalf("acceptance: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Subtests

// testDryRunSubscriptionLoop covers criterion 1 without suspension: two
// submissions and subscriptions, the first failed and the second succeeded.
func testDryRunSubscriptionLoop(t *testing.T) {
	ctx := testContext(t, 4*time.Minute)

	marker := "dryrun-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	prompt := fmt.Sprintf(
		"Submit the JSON content {\"note\":%q} to extsim for validation. "+
			"If it is rejected, correct it and resubmit, then subscribe to the result. "+
			"The content marker: %s. Report the outcome.",
		marker, marker)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration: 2 * time.Second,
		Plans: []extsimPlan{
			{DurationMS: 2000},
			{DurationMS: 2000},
		},
		Script: filepath.Join(repoRoot(t), "hack", "acceptance", "fakeclaude", "testdata", "subscription.json"),
	})
	runID := f.startRun(ctx, prompt)

	// Turn 1 submits and subscribes; turn 2 handles the failed completion and
	// resubmits; turn 3 reports the succeeded completion.
	f.h.waitTurnBoundary(ctx, runID, 3)

	c := f.h.collectDryRunCriteria(t, ctx, runID)
	c.Expect.InitialMarker = marker
	assertJudge(t, "round trip", judgeRoundTrip(c))

	f.finish(ctx, runID)
}

// testDryRunSubscriptionCallbackFirst covers criterion 2 without suspension:
// the callback arrives before the subscription is registered.
func testDryRunSubscriptionCallbackFirst(t *testing.T) {
	ctx := testContext(t, 4*time.Minute)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration: time.Second,
		Script:      filepath.Join(repoRoot(t), "hack", "acceptance", "testdata", "fake-claude-dryrun-precreated.json"),
	})
	jobs := f.h.extsimJobs()
	job, err := jobs.submitJob(ctx, dryRunExtsimProject, `{"owner":"fakeclaude"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}

	ctl := f.h.extsimControlClient()
	waitForDryRun(t, ctx, "extsim to deliver the callback", func() (bool, error) {
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
	waitForDryRun(t, ctx, "the connector event log to record the job", func() (bool, error) {
		events, err := f.h.readConnectorEvents(ctx)
		if err != nil {
			return false, err
		}
		return len(criteriaEventsForJob(events, job.ID)) > 0, nil
	})

	runID := f.startRun(ctx,
		fmt.Sprintf("Use the platform subscription tool to wait for job %s to complete, then report the outcome.", job.ID))
	f.h.waitTurnBoundary(ctx, runID, 2)

	c := f.h.collectDryRunCriteria(t, ctx, runID, job.ID)
	c.Expect.JobID = job.ID
	assertJudge(t, "callback first", judgeCallbackFirst(c))

	f.finish(ctx, runID)
}

// testDryRunSubscriptionCallbackDropped covers criterion 3 without suspension:
// the job finished but its callback was dropped, so registration's status
// query produces the completion event.
func testDryRunSubscriptionCallbackDropped(t *testing.T) {
	ctx := testContext(t, 4*time.Minute)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration: time.Second,
		Plans:       []extsimPlan{{DurationMS: 1000, Callback: "drop"}},
		Script:      filepath.Join(repoRoot(t), "hack", "acceptance", "testdata", "fake-claude-dryrun-precreated.json"),
	})
	jobs := f.h.extsimJobs()
	job, err := jobs.submitJob(ctx, dryRunExtsimProject, `{"owner":"fakeclaude"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}

	waitForDryRun(t, ctx, "the job to complete", func() (bool, error) {
		current, err := jobs.getJob(ctx, job.ID)
		if err != nil {
			return false, err
		}
		return current.Status == "completed", nil
	})

	runID := f.startRun(ctx,
		fmt.Sprintf("Use the platform subscription tool to wait for job %s to complete, then report the outcome.", job.ID))
	f.h.waitTurnBoundary(ctx, runID, 2)

	c := f.h.collectDryRunCriteria(t, ctx, runID, job.ID)
	c.Expect.JobID = job.ID
	assertJudge(t, "callback dropped", judgeCallbackDropped(c))

	f.finish(ctx, runID)
}

// testDryRunSubscriptionDuplicateDelivery covers criterion 4 without
// suspension: two callbacks with different delivery identifiers, one inbox
// row, and a same-identifier replay answered with 200.
func testDryRunSubscriptionDuplicateDelivery(t *testing.T) {
	ctx := testContext(t, 4*time.Minute)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration: 5 * time.Second,
		Plans:       []extsimPlan{{DurationMS: 5000, Callback: "duplicate"}},
		Script:      filepath.Join(repoRoot(t), "hack", "acceptance", "testdata", "fake-claude-dryrun-precreated.json"),
	})
	jobs := f.h.extsimJobs()
	job, err := jobs.submitJob(ctx, dryRunExtsimProject, `{"owner":"fakeclaude"}`)
	if err != nil {
		t.Fatalf("acceptance: submit job: %v", err)
	}

	runID := f.startRun(ctx,
		fmt.Sprintf("Use the platform subscription tool to wait for job %s to complete, then report the outcome.", job.ID))
	f.h.waitTurnBoundary(ctx, runID, 2)

	var events []connectorEventRecord
	waitForDryRun(t, ctx, "the duplicate callbacks to reach the connector event log", func() (bool, error) {
		all, err := f.h.readConnectorEvents(ctx)
		if err != nil {
			return false, err
		}
		events = criteriaEventsForJob(all, job.ID)
		return len(events) == 2, nil
	})

	status, err := f.h.replayWebhook(ctx, events[0].DeliveryID, events[0].Payload)
	if err != nil {
		t.Fatalf("acceptance: replay the webhook: %v", err)
	}
	replayStatus := status

	observationStarted := time.Now().UTC()
	select {
	case <-ctx.Done():
		t.Fatalf("acceptance: observation window: %v", ctx.Err())
	case <-time.After(criteriaDuplicateWindow + time.Second):
	}
	observationEnded := time.Now().UTC()

	c := f.h.collectDryRunCriteria(t, ctx, runID, job.ID)
	turns, err := f.h.readTurns(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read turns: %v", err)
	}
	c.Turns = turns
	c.Expect.JobID = job.ID
	c.Expect.ReplayStatus = replayStatus
	c.Times.ObservationStartedAt = observationStarted
	c.Times.ObservationEndedAt = observationEnded
	assertJudge(t, "duplicate delivery", judgeDuplicateDelivery(c))

	f.finish(ctx, runID)
}

// testDryRunSubscriptionRestart covers criterion 5 without suspension: the
// control plane stops while the job is still running, the job completes while
// it is down, extsim retries, and the restarted control plane delivers the
// completion.
func testDryRunSubscriptionRestart(t *testing.T) {
	ctx := testContext(t, 5*time.Minute)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration:     8 * time.Second,
		Script:          filepath.Join(repoRoot(t), "hack", "acceptance", "fakeclaude", "testdata", "subscription.json"),
		FixedAddrs:      true,
		ShutdownTimeout: "2s",
	})
	runID := f.startRun(ctx,
		"Submit the JSON content {\"note\":\"restart-dryrun\"} to extsim for validation. "+
			"If it is rejected, correct it and resubmit, then subscribe to the result. Report the outcome.")

	waitForDryRun(t, ctx, "the subscription to be registered", func() (bool, error) {
		subscriptions, err := f.h.readSubscriptions(ctx, runID)
		if err != nil {
			return false, err
		}
		return len(subscriptions) > 0, nil
	})
	runAtStop, err := f.h.readRun(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read run before stop: %v", err)
	}

	if err := f.h.stopChild(f.h.control, 15*time.Second); err != nil {
		t.Fatalf("acceptance: stop control plane: %v", err)
	}
	stoppedAt := time.Now().UTC()

	// The job has to finish between the stop and the restart, and extsim has
	// to record at least one failed callback attempt while the control plane
	// is down.
	subscriptions, err := f.h.readSubscriptions(ctx, runID)
	if err != nil || len(subscriptions) == 0 {
		t.Fatalf("acceptance: read subscriptions after stop: %v", err)
	}
	jobID := criteriaJobID(subscriptions[0])
	if jobID == "" {
		t.Fatal("acceptance: the subscription carries no job.id")
	}
	jobs := f.h.extsimJobs()
	waitForDryRun(t, ctx, "the job to complete while the control plane is down", func() (bool, error) {
		current, err := jobs.getJob(ctx, jobID)
		if err != nil {
			return false, err
		}
		return current.Status == "completed", nil
	})
	ctl := f.h.extsimControlClient()
	waitForDryRun(t, ctx, "a failed callback attempt", func() (bool, error) {
		attempts, err := ctl.deliveries(ctx, jobID)
		if err != nil {
			return false, err
		}
		for _, attempt := range attempts {
			if !criteriaIsSuccess(attempt.StatusCode) {
				return true, nil
			}
		}
		return false, nil
	})

	restartedAt := time.Now().UTC()
	f.h.startControlPlane(ctx, f.controlArgs...)
	listeningAt := f.h.webhookListeningTimeFromLog()

	f.h.waitTurnBoundary(ctx, runID, 2)

	c := f.h.collectDryRunCriteria(t, ctx, runID, jobID)
	c.Expect.JobID = jobID
	c.Times.ControlPlaneStoppedAt = stoppedAt
	c.Times.ControlPlaneRestartedAt = restartedAt
	c.Times.WebhookListeningAt = listeningAt
	c.Times.RunStateAtStop = runAtStop.State
	assertJudge(t, "control plane restart", judgeRestartWhileSuspended(c))

	f.finish(ctx, runID)
}

// testDryRunSubscriptionBackgroundJob covers criterion 6 without suspension:
// a background job keeps the run from suspending in a real run; here the
// shared parts are the bg_jobs empty/non-empty/empty transitions, the hold at
// a turn boundary and the spontaneous turn.
func testDryRunSubscriptionBackgroundJob(t *testing.T) {
	ctx := testContext(t, 5*time.Minute)

	f := startDryRunFixture(t, ctx, dryRunFixtureOptions{
		JobDuration: 55 * time.Second,
		Script:      filepath.Join(repoRoot(t), "hack", "acceptance", "fakeclaude", "testdata", "subscription.json"),
	})

	runID := f.h.createRun(ctx,
		"Submit a job to extsim and watch it in the background. Report when it finishes.")
	observer, err := f.h.startBGJobsObserver(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: start bg_jobs observer: %v", err)
	}
	defer observer.stop()
	f.h.startWardenLocal(runID, f.claudeBin)

	// Turn 1 starts the background watch; turn 2 is the spontaneous turn after
	// the job finishes.
	f.h.waitTurnBoundary(ctx, runID, 2)
	// The observer reads once a second, so the empty set may still be one read
	// away when the spontaneous turn ends. Stop only after it has recorded the
	// transition; otherwise the last observed state stays non-empty.
	waitForDryRun(t, ctx, "the observer to record the empty bg_jobs set", func() (bool, error) {
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
	observer.stop()
	if err := observer.Err(); err != nil {
		t.Fatalf("acceptance: bg_jobs observer stopped: %v", err)
	}

	c := f.h.collectDryRunCriteria(t, ctx, runID)
	c.BGJobs = observer.Changes()
	for _, change := range c.BGJobs {
		t.Logf("acceptance: bg_jobs change at %s: %v", change.At.Format(time.RFC3339Nano), change.Jobs)
	}
	for _, observed := range c.Turns {
		ended := "still running"
		if observed.EndedAt != nil {
			ended = observed.EndedAt.Format(time.RFC3339Nano)
		}
		t.Logf("acceptance: turn %s started %s ended %s", observed.TurnID,
			observed.StartedAt.Format(time.RFC3339Nano), ended)
	}
	assertJudge(t, "background job", judgeBackgroundJob(c))

	f.finish(ctx, runID)
}
