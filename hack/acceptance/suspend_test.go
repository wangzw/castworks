//go:build acceptance

// Suspend/resume acceptance: the milestone demo, ten cycles of
// suspend/resume, and a control plane restart while a run is suspended.
//
// The control plane runs with --provider substrate and --suspend, leaving
// --idle-grace, --scan-period and --hello-timeout at their defaults (30s, 5s
// and 60s). Each scenario creates its own database through pgtest.
//
// This file also carries the orchestration helpers it needs. They are prefixed
// with "suspend" so they stay out of the way of the other acceptance steps,
// which may be developed in parallel; harness_test.go is not modified.

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/sandboxprovider"
	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

const (
	// suspendGRPCAddress is where the actors dial back to. The integration
	// tests that need actor callbacks use the same address, so the two never
	// run at the same time.
	suspendGRPCAddress = "10.12.72.11:9090"

	// suspendWorkerNamespace holds the WorkerPool the actor template selects.
	suspendWorkerNamespace = "castworks-system"

	// Environment variables the acceptance Makefile passes through.
	suspendCredentialEnv  = "CASTWORKS_ACC_CREDENTIAL_FILE"
	suspendActorImageEnv  = "CASTWORKS_ACC_ACTOR_IMAGE"
	suspendActorCommitEnv = "CASTWORKS_ACC_ACTOR_COMMIT"

	// Run states and the actor state as ate-api reports it.
	suspendStateSuspended      = "suspended"
	suspendStateResuming       = "resuming"
	suspendActorStateSuspended = "ACTOR_STATE_SUSPENDED"

	// Timeouts stay below the make target's -timeout 90m so a deadline fails
	// the test and runs t.Cleanup instead of letting go test panic.
	suspendParentTimeout = 85 * time.Minute
	suspendDemoTimeout   = 15 * time.Minute
	// suspendTenCyclesTimeout bounds the whole TenCycles scenario, not each
	// turn. The scenario runs 11 turns and 10 suspend/resume cycles; every
	// cycle also waits for the 30 s idle grace and one snapshot plus one
	// restore. Demo measures the same actor image's snapshot_ms and restore_ms,
	// so confirm after a Demo run that this bound covers 10 × (turn time +
	// restore_ms + snapshot_ms + 30 s) plus the initial turn with a visible
	// margin. Until that measurement exists this is the plan's bound, not a
	// measured one.
	suspendTenCyclesTimeout = 45 * time.Minute
	suspendRestartTimeout   = 20 * time.Minute
	suspendLongTimeout      = 15 * time.Minute
	suspendTurnTimeout      = 10 * time.Minute
	suspendStateTimeout     = 5 * time.Minute
	suspendCPStopTimeout    = 30 * time.Second

	// suspendLongHold is how long the run stays suspended before the wake
	// message, so the guest clock and the old connections have a long freeze
	// to observe. This subtest is an observation, not a pass criterion.
	suspendLongHold = 120 * time.Second

	// suspendResumePollInterval samples the run state while a message wakes a
	// suspended run. Restore takes seconds, so 50 ms is fine enough to observe
	// the transitional resuming state without adding load.
	suspendResumePollInterval = 50 * time.Millisecond

	// suspendCycles is the number of append messages; the initial prompt is
	// the first turn, so TenCycles runs suspendCycles+1 turns.
	suspendCycles = 10

	// suspendCounterInitial is the single line the first turn writes. The
	// plan leaves the initial content to the prompt; starting at 0 keeps
	// "line count k+1, last line k" true after the k-th append and leaves
	// lines 1 to 10 each exactly once.
	suspendCounterInitial = 0
)

var suspendActorImageRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

var (
	// count=, last= and the file-proof answers. The separator accepts the
	// ASCII equals and colon plus their full-width forms, and a key or value
	// may be wrapped in * or `. Later occurrences win.
	suspendCountPattern        = regexp.MustCompile(`(?i)\*{0,2}\bcount\*{0,2}\s*[:=：＝]\s*\*{0,2}\s*(\d+)`)
	suspendLastPattern         = regexp.MustCompile(`(?i)\*{0,2}\blast\*{0,2}\s*[:=：＝]\s*\*{0,2}\s*\D{0,3}(\d+)`)
	suspendFilePattern         = regexp.MustCompile(`(?im)\*{0,2}\bfile\*{0,2}\s*[:=：＝]\s*(.*)$`)
	suspendCounterValuePattern = regexp.MustCompile("^[\\x60*\"']{0,2}\\s*(\\d+(?:\\s*[,，]\\s*\\d+)*)")
	suspendProofFilePattern    = regexp.MustCompile(`(?i)\*{0,2}\bfile\*{0,2}\s*[:=：＝]\s*\*{0,2}\s*([0-9A-Za-z]+)`)
	suspendPassPattern         = regexp.MustCompile(`(?i)\*{0,2}\bpass(?:phrase|word)?\*{0,2}\s*[:=：＝]\s*\*{0,2}\s*([A-Za-z0-9_-]+)`)
	suspendMtimePattern        = regexp.MustCompile(`(?i)\*{0,2}\bmtime\*{0,2}\s*[:=：＝]\s*\*{0,2}\s*(\d+)`)
	suspendGuestTimePattern    = regexp.MustCompile(`\d+\.\d+`)
	suspendLogTimePattern      = regexp.MustCompile(`\btime=(\S+)`)
	// The control plane's "provider call" line. The handler writes time, level
	// and msg, then the attributes in the order logProviderCall passes them.
	suspendProviderCallPattern = regexp.MustCompile(
		`\btime=(\S+)\s+level=\S+\s+msg="provider call"\s+op=(\S+)\s+run_id=(\S+)\s+duration_ms=(\d+)`)
)

// TestSuspendResume is the milestone's step 6: run the demo, check the ten
// cycle criterion, and restart the control plane while a run is suspended.
// Subtests run in order; a failing subtest skips the ones after it, because
// the next criterion assumes the previous one held.
func TestSuspendResume(t *testing.T) {
	ctx := testContext(t, suspendParentTimeout)

	requireEnv(t, envDatabaseURL)
	credentialPath := requireEnv(t, suspendCredentialEnv)
	actorImage := requireEnv(t, suspendActorImageEnv)
	// The image under test was built from its own commit. The acceptance
	// branch HEAD contains these tests, so reading HEAD would record the wrong
	// provenance.
	actorCommit := requireEnv(t, suspendActorCommitEnv)
	if !suspendActorImageRef.MatchString(actorImage) {
		t.Fatalf("acceptance: %s is %q, want name@sha256:<64 lowercase hex digits>", suspendActorImageEnv, actorImage)
	}
	// Fail before starting anything when another run holds the address actors
	// dial back to.
	suspendRequireAddrAvailable(t)

	scenarios := []struct {
		name string
		run  func(*testing.T, context.Context, string, string, string) *suspendScenarioMeasurement
	}{
		{"Demo", runSuspendDemo},
		{"TenCycles", runSuspendTenCycles},
		{"RestartWhileSuspended", runSuspendRestartWhileSuspended},
		// LongSuspension is an observation, not a pass criterion. It runs last
		// so that a functional failure in it cannot stop the criteria above
		// from running.
		{"LongSuspension", runSuspendLongSuspension},
	}

	var completed []suspendScenarioMeasurement
	failed := false
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.name, func(t *testing.T) {
			if failed {
				t.Skipf("skipping %s because an earlier subtest failed", scenario.name)
			}
			result := scenario.run(t, ctx, credentialPath, actorImage, actorCommit)
			if result != nil {
				completed = append(completed, *result)
			}
		})
		if t.Failed() {
			failed = true
		}
	}

	suspendWriteCombined(t, completed)
}

// runSuspendDemo covers the demo scenario and the first pass criterion. Every
// human-facing step goes through the castworks CLI; polling and suspension
// records go through the HTTP API, as the orchestration decision says.
func runSuspendDemo(t *testing.T, parent context.Context, credentialPath, actorImage, actorCommit string) *suspendScenarioMeasurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, suspendDemoTimeout)
	defer cancel()

	h := suspendNewHarness(t)
	m := newSuspendScenario("Demo")
	defer m.finish(t, h.workDir)
	suspendRecordProvenance(m, actorImage, actorCommit)

	suspendRequireAddrAvailable(t)
	h.suspendStartControlPlane(ctx, "controlplane", credentialPath, actorImage)

	// The capacity release criterion compares the pool's allocated resources
	// against this baseline. The baseline is read before the run exists.
	baseline, err := suspendWorkerAllocated(ctx, h)
	if err != nil {
		t.Fatalf("acceptance: read worker allocation baseline: %v", err)
	}
	m.WorkerAllocatedBaseline = &baseline

	nonce := suspendNonce()
	passphrase := suspendPassphrase()
	fileContent := suspendFileContent(nonce)
	runID := h.suspendCreateRunCLI(ctx, suspendFileProofPrompt(nonce, passphrase))
	actor := sandboxprovider.ActorName(runID)
	m.RunID = runID
	m.Actor = actor

	first := h.suspendWaitTurn(ctx, runID, 1)
	// The first turn must not echo the nonce or the hash: the second turn has
	// to read the file, not repeat what the prompt showed.
	suspendRequireNoEcho(t, first.Turns[0], 1, nonce, fileContent)
	previousTurn := first.Turns[0]
	m.addTurn(previousTurn, 1, false, nil, nil)

	suspensionDetail := suspendAwaitSuspension(ctx, h, m, runID, &previousTurn)
	suspendRequireSuspensionHealth(t, suspensionDetail.Suspensions, 1)

	info := suspendWaitActorState(ctx, h, actor, suspendActorStateSuspended)
	if info.WorkerAssigned {
		t.Fatalf("acceptance: actor %s is suspended but still assigned to worker %s", actor, info.WorkerPod)
	}
	m.ActorStateAtSuspend = info.State
	m.WorkerListing = suspendWorkerListing(ctx, h)

	atSuspend := suspendAwaitAllocatedEqual(ctx, h, baseline)
	m.WorkerAllocatedAtSuspend = &atSuspend
	tracker := newSuspendSnapshotTracker()
	suspendMeasureAndAttachSnapshot(ctx, h, m, tracker, 0, actor)

	watcher := startSuspendRunStateWatcher(ctx, h, runID)
	sentAt := time.Now()
	h.suspendSendCLI(ctx, runID, suspendFileRecallPrompt())
	second := h.suspendWaitTurn(ctx, runID, 2)
	watcher.stop()
	if len(second.Turns) < 2 {
		t.Fatalf("acceptance: run has %d turns after the second message", len(second.Turns))
	}
	secondTurn := second.Turns[1]
	suspendRequireFileProof(t, secondTurn, previousTurn, 2, fileContent, passphrase)
	m.addTurn(secondTurn, 2, true, &sentAt, nil)
	m.setSuspensions(second)
	if len(m.Suspensions) > 0 {
		m.Suspensions[0].ResumeTurnIndex = 2
		m.Suspensions[0].SentWhileSuspended = watcher.sawResuming()
		if !m.Suspensions[0].SentWhileSuspended {
			m.addNote("the second message produced no observed resuming state")
		}
	}
	suspendCheckSuspensionComplete(t, second.Suspensions, 1)

	afterResume := suspendAwaitAllocatedAbove(ctx, h, baseline)
	m.WorkerAllocatedAfterResume = &afterResume
	suspendCheckCLIGet(t, h.suspendGetCLI(ctx, runID))
	m.Connections = suspendConnectionObservations(ctx, h, actor)

	h.saveActorState(ctx, actor)
	h.suspendEndCLI(ctx, runID)
	suspendWaitActorGone(ctx, h, actor)
	h.runID = ""
	return m
}

// runSuspendTenCycles covers the second pass criterion: every message is sent
// while the run is suspended, so each one triggers exactly one restore, and
// the workspace file survives all ten.
func runSuspendTenCycles(t *testing.T, parent context.Context, credentialPath, actorImage, actorCommit string) *suspendScenarioMeasurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, suspendTenCyclesTimeout)
	defer cancel()

	h := suspendNewHarness(t)
	m := newSuspendScenario("TenCycles")
	defer m.finish(t, h.workDir)
	suspendRecordProvenance(m, actorImage, actorCommit)

	suspendRequireAddrAvailable(t)
	h.suspendStartControlPlane(ctx, "controlplane", credentialPath, actorImage)

	runID := h.createRun(ctx, suspendCounterInitialPrompt())
	actor := sandboxprovider.ActorName(runID)
	m.RunID = runID
	m.Actor = actor

	detail := h.suspendWaitTurn(ctx, runID, 1)
	suspendRequireCounterOutput(t, detail.Turns[0], 1, suspendCounterInitial)
	previousTurn := detail.Turns[0]
	m.addTurn(previousTurn, 1, false, nil, nil)

	// status.snapshots keeps the current and the in-progress generation, so
	// each measurement after a suspension contains this cycle's snapshot plus
	// possibly the previous one. The tracker picks only the unseen URI.
	tracker := newSuspendSnapshotTracker()

	for k := 1; k <= suspendCycles; k++ {
		suspended := suspendAwaitSuspension(ctx, h, m, runID, &previousTurn)
		// A failed suspension must stop the scenario immediately; later
		// assertions would otherwise report a misleading turn mismatch. The
		// record count also proves each cycle produced exactly one suspension.
		suspendRequireSuspensionHealth(t, suspended.Suspensions, k)
		suspendMeasureAndAttachSnapshot(ctx, h, m, tracker, k-1, actor)

		watcher := startSuspendRunStateWatcher(ctx, h, runID)
		sentAt := time.Now()
		h.send(ctx, runID, suspendCounterCyclePrompt(k))
		detail = h.suspendWaitTurn(ctx, runID, k+1)
		watcher.stop()
		if len(detail.Turns) < k+1 {
			t.Fatalf("acceptance: run has %d turns, want at least %d", len(detail.Turns), k+1)
		}
		observed := detail.Turns[k]
		suspendRequireCounterOutput(t, observed, k+1, k)

		var skew *suspendClockSkew
		if guest, ok := suspendParseGuestTime(observed.Output); ok && observed.EndedAt != nil {
			value := suspendClockSkewFor(guest, observed)
			skew = &value
		} else {
			m.addNote("turn %d: no guest clock reading found; clock skew not measured", k+1)
		}
		m.addTurn(observed, k+1, true, &sentAt, skew)
		if k == suspendCycles {
			suspendRequireCounterFile(t, observed.Output, m)
		}

		m.setSuspensions(detail)
		if k-1 < len(m.Suspensions) {
			m.Suspensions[k-1].ResumeTurnIndex = k + 1
			m.Suspensions[k-1].SentWhileSuspended = watcher.sawResuming()
			if !m.Suspensions[k-1].SentWhileSuspended {
				m.addNote("cycle %d: the message produced no observed resuming state", k)
			}
		}
		previousTurn = observed
	}

	if len(detail.Turns) != suspendCycles+1 {
		t.Fatalf("acceptance: run has %d turns, want %d", len(detail.Turns), suspendCycles+1)
	}
	for i, observed := range detail.Turns {
		if observed.IsError {
			t.Fatalf("acceptance: turn %d is an error: %q", i+1, observed.Output)
		}
		if observed.EndedAt == nil {
			t.Fatalf("acceptance: turn %d has not ended", i+1)
		}
	}
	suspendCheckSuspensionComplete(t, detail.Suspensions, suspendCycles)

	m.Connections = suspendConnectionObservations(ctx, h, actor)

	h.saveActorState(ctx, actor)
	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("acceptance: end run %s: %v", runID, err)
	}
	suspendWaitActorGone(ctx, h, actor)
	h.runID = ""
	return m
}

// runSuspendRestartWhileSuspended covers the third pass criterion: the control
// plane is stopped with SIGTERM while the run is suspended, the actor stays
// SUSPENDED, and a restarted control plane on the same database resumes it.
func runSuspendRestartWhileSuspended(t *testing.T, parent context.Context, credentialPath, actorImage, actorCommit string) *suspendScenarioMeasurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, suspendRestartTimeout)
	defer cancel()

	h := suspendNewHarness(t)
	m := newSuspendScenario("RestartWhileSuspended")
	defer m.finish(t, h.workDir)
	suspendRecordProvenance(m, actorImage, actorCommit)

	suspendRequireAddrAvailable(t)
	h.suspendStartControlPlane(ctx, "controlplane", credentialPath, actorImage)

	nonce := suspendNonce()
	passphrase := suspendPassphrase()
	fileContent := suspendFileContent(nonce)
	runID := h.createRun(ctx, suspendFileProofPrompt(nonce, passphrase))
	actor := sandboxprovider.ActorName(runID)
	m.RunID = runID
	m.Actor = actor

	first := h.suspendWaitTurn(ctx, runID, 1)
	suspendRequireNoEcho(t, first.Turns[0], 1, nonce, fileContent)
	previousTurn := first.Turns[0]
	m.addTurn(previousTurn, 1, false, nil, nil)

	suspensionDetail := suspendAwaitSuspension(ctx, h, m, runID, &previousTurn)
	suspendRequireSuspensionHealth(t, suspensionDetail.Suspensions, 1)

	info := suspendWaitActorState(ctx, h, actor, suspendActorStateSuspended)
	if info.WorkerAssigned {
		t.Fatalf("acceptance: actor %s is suspended but still assigned to worker %s", actor, info.WorkerPod)
	}
	m.ActorStateAtSuspend = info.State
	m.WorkerListing = suspendWorkerListing(ctx, h)

	tracker := newSuspendSnapshotTracker()
	suspendMeasureAndAttachSnapshot(ctx, h, m, tracker, 0, actor)

	// Stop the control plane with SIGTERM and require exit status 0.
	suspendStopControlPlane(t, h, h.control)
	restartAt := time.Now().UTC()
	m.ControlPlaneRestarts = 1
	m.ControlPlaneRestartAt = &restartAt

	// The actor must remain suspended without a control plane watching it.
	suspendWaitActorState(ctx, h, actor, suspendActorStateSuspended)

	suspendRequireAddrAvailable(t)
	h.suspendStartControlPlane(ctx, "controlplane-restart", credentialPath, actorImage)

	// Before any message, the restarted control plane must still report the
	// run as suspended. A run that silently resumed on restart would fail here
	// instead of showing up only as a missing resuming observation.
	suspendRequireRunState(ctx, h, runID, suspendStateSuspended)

	watcher := startSuspendRunStateWatcher(ctx, h, runID)
	sentAt := time.Now()
	h.send(ctx, runID, suspendFileRecallPrompt())
	second := h.suspendWaitTurn(ctx, runID, 2)
	watcher.stop()
	if len(second.Turns) < 2 {
		t.Fatalf("acceptance: run has %d turns after the restart", len(second.Turns))
	}
	secondTurn := second.Turns[1]
	suspendRequireFileProof(t, secondTurn, previousTurn, 2, fileContent, passphrase)
	m.addTurn(secondTurn, 2, true, &sentAt, nil)
	m.setSuspensions(second)
	if len(m.Suspensions) > 0 {
		m.Suspensions[0].ResumeTurnIndex = 2
		m.Suspensions[0].SentWhileSuspended = watcher.sawResuming()
		if !m.Suspensions[0].SentWhileSuspended {
			m.addNote("the message after the restart produced no observed resuming state")
		}
	}
	suspendCheckSuspensionComplete(t, second.Suspensions, 1)

	m.Connections = suspendConnectionObservations(ctx, h, actor)

	h.saveActorState(ctx, actor)
	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("acceptance: end run %s: %v", runID, err)
	}
	suspendWaitActorGone(ctx, h, actor)
	h.runID = ""
	return m
}

// runSuspendLongSuspension keeps the run suspended for a long interval before
// sending the wake message. It is the Demo's two turns, so it is functional on
// the same terms, but its purpose is observation: the frozen interval gives a
// guest clock reading and old-connection log lines that a short suspension
// cannot. It is not a pass criterion.
func runSuspendLongSuspension(t *testing.T, parent context.Context, credentialPath, actorImage, actorCommit string) *suspendScenarioMeasurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, suspendLongTimeout)
	defer cancel()

	h := suspendNewHarness(t)
	m := newSuspendScenario("LongSuspension")
	defer m.finish(t, h.workDir)
	suspendRecordProvenance(m, actorImage, actorCommit)

	suspendRequireAddrAvailable(t)
	h.suspendStartControlPlane(ctx, "controlplane", credentialPath, actorImage)

	nonce := suspendNonce()
	passphrase := suspendPassphrase()
	fileContent := suspendFileContent(nonce)
	runID := h.suspendCreateRunCLI(ctx, suspendFileProofPrompt(nonce, passphrase))
	actor := sandboxprovider.ActorName(runID)
	m.RunID = runID
	m.Actor = actor

	first := h.suspendWaitTurn(ctx, runID, 1)
	suspendRequireNoEcho(t, first.Turns[0], 1, nonce, fileContent)
	previousTurn := first.Turns[0]
	m.addTurn(previousTurn, 1, false, nil, nil)

	suspensionDetail := suspendAwaitSuspension(ctx, h, m, runID, &previousTurn)
	suspendRequireSuspensionHealth(t, suspensionDetail.Suspensions, 1)

	info := suspendWaitActorState(ctx, h, actor, suspendActorStateSuspended)
	if info.WorkerAssigned {
		t.Fatalf("acceptance: actor %s is suspended but still assigned to worker %s", actor, info.WorkerPod)
	}
	m.ActorStateAtSuspend = info.State

	tracker := newSuspendSnapshotTracker()
	suspendMeasureAndAttachSnapshot(ctx, h, m, tracker, 0, actor)

	// Stay suspended long enough that the guest clock's behaviour and the old
	// connections' timing are visible against the freeze.
	holdStarted := time.Now()
	if err := suspendSleep(ctx, suspendLongHold); err != nil {
		t.Fatalf("acceptance: wait %s with the run suspended: %v", suspendLongHold, err)
	}
	holdMS := time.Since(holdStarted).Milliseconds()

	watcher := startSuspendRunStateWatcher(ctx, h, runID)
	sentAt := time.Now()
	h.suspendSendCLI(ctx, runID, suspendLongRecallPrompt())
	second := h.suspendWaitTurn(ctx, runID, 2)
	watcher.stop()

	// The scenario sent one message. Exactly two ended turns is the
	// observable form of "delivered exactly once": a duplicate delivery would
	// open another turn.
	if len(second.Turns) != 2 {
		t.Fatalf("acceptance: run has %d turns after one wake message, want exactly 2", len(second.Turns))
	}
	secondTurn := second.Turns[1]
	suspendRequireFileProof(t, secondTurn, previousTurn, 2, fileContent, passphrase)

	var skew *suspendClockSkew
	if guest, ok := suspendParseGuestTime(secondTurn.Output); ok && secondTurn.EndedAt != nil {
		value := suspendClockSkewFor(guest, secondTurn)
		skew = &value
	} else {
		m.addNote("no guest clock reading found in the second turn; clock skew not measured")
	}
	m.addTurn(secondTurn, 2, true, &sentAt, skew)

	m.setSuspensions(second)
	if len(m.Suspensions) > 0 {
		m.Suspensions[0].ResumeTurnIndex = 2
		m.Suspensions[0].SentWhileSuspended = watcher.sawResuming()
		if !m.Suspensions[0].SentWhileSuspended {
			m.addNote("the message after the long hold produced no observed resuming state")
		}
	}
	suspendCheckSuspensionComplete(t, second.Suspensions, 1)

	m.Connections = suspendConnectionObservations(ctx, h, actor)
	m.Timeline = suspendLongTimeline(ctx, t, h, actor, second.Suspensions[0], holdMS, sentAt)
	if m.Timeline.FrozenMS == nil {
		m.addNote("the frozen interval could not be derived; see the timeline source")
	}

	h.saveActorState(ctx, actor)
	h.suspendEndCLI(ctx, runID)
	suspendWaitActorGone(ctx, h, actor)
	h.runID = ""
	return m
}

// suspendNewHarness creates a fresh database and a harness for one scenario.
// The database cleanup registered by pgtest runs after the harness cleanup, so
// the control plane stops before its database is dropped.
func suspendNewHarness(t *testing.T) *harness {
	t.Helper()
	pool := pgtest.NewPool(t)
	h := newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "substrate",
	})
	h.buildBinaries()
	return h
}

// suspendRecordProvenance writes the actor image digest and the commit the
// image was built from into the measurement. The commit is passed in by the
// Makefile; the acceptance branch HEAD is not the image's source, because the
// acceptance tests are committed after the image was built.
func suspendRecordProvenance(m *suspendScenarioMeasurement, actorImage, actorCommit string) {
	m.ActorImage = actorImage
	m.RepoCommit = actorCommit
	if actorCommit == "" {
		m.addNote("the actor image source commit was not provided")
	}
}

// suspendRequireAddrAvailable checks that the actors' callback address can be
// bound. A busy address means another acceptance or integration test is
// running; starting the control plane would only hang until its dial timeout.
func suspendRequireAddrAvailable(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", suspendGRPCAddress)
	if err != nil {
		t.Fatalf("acceptance: %s is not available: %v (another integration test that needs actor callbacks may be running)", suspendGRPCAddress, err)
	}
	_ = listener.Close()
}

func suspendControlPlaneArgs(credentialPath, actorImage, kubeContext string) []string {
	args := []string{
		"--provider", "substrate",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", suspendGRPCAddress,
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialPath,
		"--substrate-image", actorImage,
		"--suspend",
	}
	if kubeContext != "" {
		args = append(args, "--substrate-context", kubeContext)
	}
	return args
}

// suspendStartControlPlane starts a control plane under name, waits for both
// listeners and records their addresses on the harness. A restart uses a
// different name so the first process's logs are not truncated.
func (h *harness) suspendStartControlPlane(ctx context.Context, name, credentialPath, actorImage string) *childProcess {
	h.t.Helper()
	args := suspendControlPlaneArgs(credentialPath, actorImage, h.kubeCtx)
	full := append([]string(nil), args...)
	if h.database != "" {
		full = append(full, "--database-url", h.database)
	}
	child := h.startChild(name, filepath.Join(h.binDir, "castworks-controlplane"), full)
	h.control = child

	paths := []string{
		filepath.Join(h.workDir, name+".stderr.log"),
		filepath.Join(h.workDir, name+".stdout.log"),
	}
	httpAddr, err := waitForAddr(ctx, paths, "http listening")
	if err != nil {
		h.t.Fatalf("acceptance: read control plane HTTP address: %v", err)
	}
	h.httpAddr = httpAddr
	grpcAddr, err := waitForAddr(ctx, paths, "grpc listening")
	if err != nil {
		h.t.Fatalf("acceptance: read control plane gRPC address: %v", err)
	}
	h.grpcAddr = grpcAddr
	return child
}

// suspendStopControlPlane sends SIGTERM and requires a clean exit. Exit
// status 0 is the pass criterion for the restart scenario, so a non-zero
// status fails instead of being retried.
func suspendStopControlPlane(t *testing.T, h *harness, child *childProcess) {
	t.Helper()
	if child == nil || child.exited() {
		t.Fatalf("acceptance: control plane is not running")
	}
	if err := signalProcess(child.cmd.Process.Pid, syscall.SIGTERM); err != nil && !isProcessGone(err) {
		t.Fatalf("acceptance: signal control plane: %v", err)
	}
	if err := h.waitChild(child, suspendCPStopTimeout); err != nil {
		t.Fatalf("acceptance: control plane did not exit with status 0 after SIGTERM: %v", err)
	}
}

// suspendRunCLI runs the castworks CLI against this harness's HTTP API.
func suspendRunCLI(ctx context.Context, h *harness, args ...string) string {
	h.t.Helper()
	full := append([]string{"--server", "http://" + h.httpAddr}, args...)
	cmd := exec.CommandContext(ctx, filepath.Join(h.binDir, "castworks"), full...)
	cmd.Env = childEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("acceptance: castworks %s: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(full, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// suspendCreateRunCLI creates a run through the CLI, keeps the ledger entry
// the harness would have written, and returns the run ID.
func (h *harness) suspendCreateRunCLI(ctx context.Context, prompt string) string {
	h.t.Helper()
	out := suspendRunCLI(ctx, h, "run", "create", prompt)
	runID := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if runID == "" {
		h.t.Fatalf("acceptance: run create printed no run_id: %q", out)
	}
	h.runID = runID
	entry := ledgerEntry{
		Time:     time.Now().UTC(),
		RunID:    runID,
		WorkDir:  h.workDir,
		Provider: h.provider,
	}
	if err := appendLedger(entry); err != nil {
		h.t.Fatalf("acceptance: record run %s in ledger: %v", runID, err)
	}
	return runID
}

func (h *harness) suspendSendCLI(ctx context.Context, runID, text string) {
	h.t.Helper()
	suspendRunCLI(ctx, h, "run", "send", runID, text)
}

func (h *harness) suspendGetCLI(ctx context.Context, runID string) string {
	h.t.Helper()
	return suspendRunCLI(ctx, h, "run", "get", runID)
}

func (h *harness) suspendEndCLI(ctx context.Context, runID string) {
	h.t.Helper()
	suspendRunCLI(ctx, h, "run", "end", runID)
}

// suspendWaitTurn waits for n ended turns and the boundary, with a per-turn
// deadline shorter than the whole scenario's.
func (h *harness) suspendWaitTurn(ctx context.Context, runID string, n int) runDetail {
	h.t.Helper()
	turnCtx, cancel := context.WithTimeout(ctx, suspendTurnTimeout)
	defer cancel()
	return h.waitTurnBoundary(turnCtx, runID, n)
}

// suspendAwaitSuspension waits until the run is suspended and its newest
// suspension record is visible with snapshot_ms. Waiting for the record too
// keeps a state change and its record from being read in separate requests
// with an empty result in between.
func suspendAwaitSuspension(ctx context.Context, h *harness, m *suspendScenarioMeasurement, runID string, previous *turn) runDetail {
	h.t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, suspendStateTimeout)
	defer cancel()

	var detail runDetail
	var lastErr error
	err := waitFor(waitCtx, fmt.Sprintf("run %s to suspend with a complete suspension record", runID), func() (bool, error) {
		current, err := h.getRun(waitCtx, runID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if current.State == "failed" {
			return false, fmt.Errorf("run %s failed: %s", runID, current.FailureReason)
		}
		detail = current
		// A suspension record with an error means the snapshot or the restore
		// failed; waiting for a timing that will never arrive would hide that
		// until the timeout.
		for i, record := range current.Suspensions {
			if record.Error != "" {
				return false, fmt.Errorf("suspension %d failed: %s", i+1, record.Error)
			}
		}
		if current.State != suspendStateSuspended || len(current.Suspensions) == 0 {
			return false, nil
		}
		latest := current.Suspensions[len(current.Suspensions)-1]
		return latest.SnapshotMS != nil, nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v (last read error: %v)", err, lastErr)
	}

	m.setSuspensions(detail)
	m.markSuspensionDetected(len(detail.Suspensions)-1, time.Now().UTC(), previous)
	return detail
}

// suspendActorInfo is the subset of the actor JSON this step reads.
type suspendActorInfo struct {
	State          string
	WorkerAssigned bool
	WorkerPod      string
}

var suspendActorStateNames = map[int]string{
	0: "ACTOR_STATE_UNSPECIFIED",
	1: "ACTOR_STATE_RESUMING",
	2: "ACTOR_STATE_RUNNING",
	3: "ACTOR_STATE_SUSPENDING",
	4: "ACTOR_STATE_SUSPENDED",
	5: "ACTOR_STATE_CRASHED",
	6: "ACTOR_STATE_DELETING",
	7: "ACTOR_STATE_REVERTING",
}

func suspendGetActor(ctx context.Context, kubeContext, actor string) (suspendActorInfo, error) {
	out, err := kubectlAte(ctx, kubeContext, "get", "actor", actor, "-a", ateSpace, "-o", "json")
	if err != nil {
		return suspendActorInfo{}, err
	}
	var doc struct {
		Status struct {
			State            json.RawMessage `json:"state"`
			WorkerAssignment json.RawMessage `json:"workerAssignment"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return suspendActorInfo{}, fmt.Errorf("decoding actor %s JSON: %w", actor, err)
	}
	info := suspendActorInfo{State: suspendDecodeActorState(doc.Status.State)}
	assignment := strings.TrimSpace(string(doc.Status.WorkerAssignment))
	if assignment != "" && assignment != "null" && assignment != "{}" {
		info.WorkerAssigned = true
		var worker struct {
			WorkerPod string `json:"workerPod"`
		}
		if err := json.Unmarshal(doc.Status.WorkerAssignment, &worker); err == nil {
			info.WorkerPod = worker.WorkerPod
		}
	}
	return info, nil
}

// suspendDecodeActorState accepts both protojson's enum name and a bare
// number, so a change of kubectl's output encoding does not quietly turn every
// state into an empty string.
func suspendDecodeActorState(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return name
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		if name, ok := suspendActorStateNames[number]; ok {
			return name
		}
	}
	return strings.Trim(string(raw), `"`)
}

// suspendWaitActorState polls kubectl until the actor reaches state.
func suspendWaitActorState(ctx context.Context, h *harness, actor, state string) suspendActorInfo {
	h.t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, suspendStateTimeout)
	defer cancel()

	var info suspendActorInfo
	var lastErr error
	err := waitFor(waitCtx, fmt.Sprintf("actor %s to reach %s", actor, state), func() (bool, error) {
		current, err := suspendGetActor(waitCtx, h.kubeCtx, actor)
		if err != nil {
			lastErr = err
			return false, nil
		}
		info = current
		return current.State == state, nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v (last read error: %v)", err, lastErr)
	}
	return info
}

// suspendWorkerListing records what the cluster says about its workers. The
// authoritative statement that the actor has no worker is
// status.workerAssignment being unset, which suspendGetActor checks; this
// listing is kept as supporting evidence for the report.
func suspendWorkerListing(ctx context.Context, h *harness) string {
	out, err := kubectlAte(ctx, h.kubeCtx, "get", "worker", "-n", suspendWorkerNamespace)
	if err != nil {
		return fmt.Sprintf("kubectl ate get worker -n %s: %v", suspendWorkerNamespace, err)
	}
	return out
}

func suspendWaitActorGone(ctx context.Context, h *harness, actor string) {
	h.t.Helper()
	goneCtx, cancel := context.WithTimeout(ctx, actorGoneTimeout)
	defer cancel()
	if err := waitActorGone(goneCtx, h.kubeCtx, actor, actorGoneTimeout); err != nil {
		h.t.Fatalf("acceptance: %v", err)
	}
}

func suspendMeasureSnapshots(ctx context.Context, h *harness, actor string) ([]snapshotMeasurement, error) {
	measureCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return newDefaultSnapshotSizer().measureActor(measureCtx, ateSpace, actor)
}

// suspendConnectionObservations collects reconnect, Hello and retry lines from
// the actor's logs. They describe how quickly old connections were noticed;
// they are observations only and never fail the test.
func suspendConnectionObservations(ctx context.Context, h *harness, actor string) []suspendConnectionObservation {
	logCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	logs, err := kubectlAte(logCtx, h.kubeCtx, "logs", "actor", actor, "-a", ateSpace)
	if err != nil {
		return []suspendConnectionObservation{{Covers: "kubectl ate logs", Line: err.Error()}}
	}

	var observations []suspendConnectionObservation
	for _, line := range strings.Split(logs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lower := strings.ToLower(line)
		relevant := strings.Contains(lower, "reconnect") ||
			strings.Contains(lower, "control stream closed") ||
			strings.Contains(lower, "dial failed") ||
			strings.Contains(lower, "opening control stream") ||
			strings.Contains(lower, "hello_ack") ||
			strings.Contains(lower, "retry") ||
			strings.Contains(lower, "econnreset") ||
			strings.Contains(lower, "connection reset")
		if !relevant {
			continue
		}
		observations = append(observations, suspendConnectionObservation{
			Covers: "warden and Claude Code logs",
			Line:   suspendTruncate(line, 2000),
			Time:   suspendParseLogTime(line),
		})
		if len(observations) >= 200 {
			break
		}
	}
	if len(observations) == 0 {
		observations = append(observations, suspendConnectionObservation{
			Covers: "warden and Claude Code logs",
			Line:   "no reconnect, Hello or retry lines found",
		})
	}
	return observations
}

func suspendParseLogTime(line string) *time.Time {
	match := suspendLogTimePattern.FindStringSubmatch(line)
	if match == nil {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.Trim(match[1], `"`))
	if err != nil {
		return nil
	}
	return &parsed
}

func suspendTruncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// suspendSleep waits for d unless ctx is cancelled first.
func suspendSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// suspendLongRecallPrompt adds a guest clock reading to the Demo recall
// prompt. The reading after a long freeze is what shows whether the guest
// clock stopped during the freeze or jumped to the current time.
func suspendLongRecallPrompt() string {
	return "先执行 date -u +%s.%N，把输出放在回复的第一行。" + suspendFileRecallPrompt()
}

// suspendProviderCall is one "provider call" line from the control plane log.
// At is when the line was written, right after the call returned; subtracting
// DurationMS gives the call's start.
type suspendProviderCall struct {
	Op         string
	At         time.Time
	DurationMS int64
}

func (c suspendProviderCall) startedAt() time.Time {
	return c.At.Add(-time.Duration(c.DurationMS) * time.Millisecond)
}

// suspendProviderCalls reads the control plane's "provider call" lines for one
// run. An unreadable log or a malformed line is not fatal: the timeline has a
// fallback derived from the suspension record.
func suspendProviderCalls(t *testing.T, path, runID string) []suspendProviderCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("acceptance: read provider call log %s: %v", path, err)
		return nil
	}
	var calls []suspendProviderCall
	for _, line := range strings.Split(string(data), "\n") {
		match := suspendProviderCallPattern.FindStringSubmatch(line)
		if match == nil || match[3] != runID {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, match[1])
		if err != nil {
			continue
		}
		duration, err := strconv.ParseInt(match[4], 10, 64)
		if err != nil {
			continue
		}
		calls = append(calls, suspendProviderCall{Op: match[2], At: at, DurationMS: duration})
	}
	return calls
}

// suspendLongTimeline derives the instants that bound the long freeze. The
// control plane's provider call log supplies snapshot completion, restore
// start and restore return; the suspension record supplies the suspension
// start. When the log is missing or does not cover the call, the record's
// durations give a fallback and Source says so.
func suspendLongTimeline(ctx context.Context, t *testing.T, h *harness, actor string, record suspension, holdMS int64, sentAt time.Time) *suspendObservationTimeline {
	t.Helper()
	timeline := &suspendObservationTimeline{
		SuspendStartedAt: record.StartedAt.UTC(),
		HoldMS:           holdMS,
		MessageSentAt:    sentAt.UTC(),
	}
	snapshotSource := "provider call log"
	restoreSource := "provider call log"

	for _, call := range suspendProviderCalls(t, filepath.Join(h.workDir, "controlplane.stderr.log"), h.runID) {
		switch call.Op {
		case "snapshot":
			if timeline.SnapshotCompletedAt == nil {
				at := call.At.UTC()
				timeline.SnapshotCompletedAt = &at
			}
		case "restore":
			if timeline.RestoreReturnedAt == nil {
				returned := call.At.UTC()
				started := call.startedAt().UTC()
				timeline.RestoreReturnedAt = &returned
				timeline.ResumeStartedAt = &started
			}
		}
	}

	if timeline.SnapshotCompletedAt == nil {
		if record.SnapshotMS != nil {
			at := record.StartedAt.Add(time.Duration(*record.SnapshotMS) * time.Millisecond).UTC()
			timeline.SnapshotCompletedAt = &at
			snapshotSource = "suspension record: started_at + snapshot_ms"
		} else {
			snapshotSource = "unavailable: snapshot_ms missing"
		}
	}
	if timeline.RestoreReturnedAt == nil {
		if record.RestoreMS != nil {
			at := sentAt.Add(time.Duration(*record.RestoreMS) * time.Millisecond).UTC()
			started := sentAt.UTC()
			timeline.RestoreReturnedAt = &at
			timeline.ResumeStartedAt = &started
			restoreSource = "suspension record: message send + restore_ms"
		} else {
			restoreSource = "unavailable: restore_ms missing"
		}
	}
	timeline.Source = fmt.Sprintf("snapshot: %s; restore: %s", snapshotSource, restoreSource)

	if timeline.SnapshotCompletedAt != nil && timeline.RestoreReturnedAt != nil {
		frozen := timeline.RestoreReturnedAt.Sub(timeline.SuspendStartedAt).Milliseconds()
		snapshotToRestore := timeline.RestoreReturnedAt.Sub(*timeline.SnapshotCompletedAt).Milliseconds()
		timeline.FrozenMS = &frozen
		timeline.SnapshotToRestoreMS = &snapshotToRestore
	}
	if timeline.RestoreReturnedAt != nil && record.HelloMS != nil {
		hello := timeline.RestoreReturnedAt.Add(time.Duration(*record.HelloMS) * time.Millisecond).UTC()
		timeline.HelloAt = &hello
		timeline.HelloFromRestoreMS = suspendOffsetMS(hello, *timeline.RestoreReturnedAt)
		timeline.HelloFromSuspendMS = suspendOffsetMS(hello, timeline.SuspendStartedAt)
	}

	suspendFillWardenTimeline(ctx, h, actor, timeline)
	return timeline
}

// suspendFillWardenTimeline records the old stream's disconnect and the first
// warden log line after the restore returned, both relative to the suspension
// start and the restore return. The warden logs the stream closing but has no
// explicit reconnect line, so the first post-restore line bounds the
// reconnection from above.
func suspendFillWardenTimeline(ctx context.Context, h *harness, actor string, timeline *suspendObservationTimeline) {
	h.t.Helper()
	logCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	logs, err := kubectlAte(logCtx, h.kubeCtx, "logs", "actor", actor, "-a", ateSpace)
	if err != nil {
		timeline.Source += "; warden log unreadable"
		return
	}

	type logEntry struct {
		at   time.Time
		line string
	}
	var entries []logEntry
	for _, line := range strings.Split(logs, "\n") {
		at := suspendParseLogTime(line)
		if at == nil {
			continue
		}
		entries = append(entries, logEntry{at: at.UTC(), line: line})
	}

	var disconnect *logEntry
	for i := range entries {
		if !suspendIsDisconnectLine(entries[i].line) {
			continue
		}
		if disconnect == nil ||
			suspendAbsDuration(entries[i].at.Sub(timeline.SuspendStartedAt)) <
				suspendAbsDuration(disconnect.at.Sub(timeline.SuspendStartedAt)) {
			disconnect = &entries[i]
		}
	}
	if disconnect != nil {
		at := disconnect.at
		timeline.DisconnectAt = &at
		timeline.DisconnectLine = suspendTruncate(disconnect.line, 2000)
		timeline.DisconnectFromSuspendMS = suspendOffsetMS(at, timeline.SuspendStartedAt)
		timeline.DisconnectFromRestoreMS = suspendOffsetFrom(at, timeline.RestoreReturnedAt)
	}

	if timeline.RestoreReturnedAt == nil {
		return
	}
	for i := range entries {
		if entries[i].at.Before(*timeline.RestoreReturnedAt) {
			continue
		}
		at := entries[i].at
		timeline.ReconnectAt = &at
		timeline.ReconnectLine = suspendTruncate(entries[i].line, 2000)
		timeline.ReconnectFromSuspendMS = suspendOffsetMS(at, timeline.SuspendStartedAt)
		timeline.ReconnectFromRestoreMS = suspendOffsetMS(at, *timeline.RestoreReturnedAt)
		break
	}
}

func suspendIsDisconnectLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "control stream closed") ||
		strings.Contains(lower, "dial failed") ||
		strings.Contains(lower, "opening control stream failed") ||
		strings.Contains(lower, "reconnecting")
}

func suspendAbsDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func suspendOffsetMS(at, base time.Time) *int64 {
	value := at.Sub(base).Milliseconds()
	return &value
}

func suspendOffsetFrom(at time.Time, base *time.Time) *int64 {
	if base == nil {
		return nil
	}
	return suspendOffsetMS(at, *base)
}

func suspendNonce() string {
	return "marker-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func suspendPassphrase() string {
	return "pass-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// suspendFileContent is the value the first turn writes to result.txt:
// echo appends a newline, sha256sum hashes it, and cut keeps the first 16 hex
// characters. The test computes it itself so the second turn's answer proves
// the file survived rather than repeating a value from the prompt.
func suspendFileContent(nonce string) string {
	sum := sha256.Sum256([]byte(nonce + "\n"))
	return hex.EncodeToString(sum[:])[:16]
}

// suspendFileProofPrompt writes the hash of a nonce to result.txt without
// echoing it. The passphrase is meant to stay in the session only; the second
// turn asks for both, which proves the session survived without naming the
// file.
func suspendFileProofPrompt(nonce, passphrase string) string {
	return fmt.Sprintf(
		"在当前工作目录里创建文件 result.txt。只执行这一条命令：echo %s | sha256sum | cut -c1-16 > result.txt。不要回显这条命令或它的输出，也不要把 %s 写进任何文件或回复。另外记住暗号 %s，它只留在会话里，不要写进文件。只回复一句：已完成。",
		nonce, nonce, passphrase)
}

// suspendFileRecallPrompt names neither the file nor the passphrase: only the
// session can supply them. It asks for the modification time as well, so the
// second turn proves it read the file instead of recreating it.
func suspendFileRecallPrompt() string {
	return "不要重新生成或修改这个文件，只读取。" +
		"把你刚才创建的那个文件的内容原样回复；再另起一行回复我第一回合给你的暗号；" +
		"再另起一行，对刚才那个文件执行 stat -c %Y，回复它打印的秒数。" +
		"按字面格式回复三行：file=<文件内容>，pass=<暗号>，mtime=<秒>。"
}

func suspendRequireNoEcho(t *testing.T, observed turn, index int, forbidden ...string) {
	t.Helper()
	if observed.IsError {
		t.Fatalf("acceptance: turn %d is an error: %q", index, observed.Output)
	}
	lower := strings.ToLower(observed.Output)
	for _, value := range forbidden {
		if value == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(value)) {
			t.Fatalf("acceptance: turn %d echoed %q; the value must come from the file, not the prompt:\n%s",
				index, value, observed.Output)
		}
	}
}

// suspendFileModTimeMargin absorbs the delay between the first turn ending
// and the model finishing the command that writes the file.
const suspendFileModTimeMargin = 5 * time.Second

// suspendFileProofReply is the second turn's answer in the file-proof
// scenario. The presence flags distinguish a missing answer from an empty
// one.
type suspendFileProofReply struct {
	FileContent string
	Passphrase  string
	ModTime     int64
	HasContent  bool
	HasPass     bool
	HasModTime  bool
}

// suspendParseFileProof extracts the three labeled answers. Later occurrences
// win, so an early echo of the instruction cannot override the actual answer.
func suspendParseFileProof(output string) suspendFileProofReply {
	var reply suspendFileProofReply
	if value, ok := suspendLastReplyValue(suspendProofFilePattern, output); ok {
		reply.FileContent, reply.HasContent = value, true
	}
	if value, ok := suspendLastReplyValue(suspendPassPattern, output); ok {
		reply.Passphrase, reply.HasPass = value, true
	}
	if value, ok := suspendLastReplyValue(suspendMtimePattern, output); ok {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			reply.ModTime, reply.HasModTime = seconds, true
		}
	}
	return reply
}

// suspendLastReplyValue returns the last captured value for a pattern.
func suspendLastReplyValue(pattern *regexp.Regexp, output string) (string, bool) {
	matches := pattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return "", false
	}
	return matches[len(matches)-1][1], true
}

func suspendRequireFileProof(t *testing.T, observed, first turn, index int, fileContent, passphrase string) {
	t.Helper()
	if observed.IsError {
		t.Fatalf("acceptance: turn %d is an error: %q", index, observed.Output)
	}
	mismatches := suspendFileProofMismatches(observed, first, fileContent, passphrase)
	if len(mismatches) == 0 {
		return
	}
	t.Fatalf("acceptance: turn %d does not prove the file survived:\n- %s\noutput:\n%s",
		index, strings.Join(mismatches, "\n- "), observed.Output)
}

// suspendFileProofMismatches lists every check the second turn failed. A file
// that was recreated after the first turn ends carries a later modification
// time, so the bound is the first turn's ended_at plus a small margin. It is
// deliberately not the suspension start: recreating the file before the
// suspend would still pass that comparison.
func suspendFileProofMismatches(observed, first turn, fileContent, passphrase string) []string {
	var mismatches []string
	reply := suspendParseFileProof(observed.Output)
	if !reply.HasContent || !strings.EqualFold(reply.FileContent, fileContent) {
		mismatches = append(mismatches,
			fmt.Sprintf("file content: got %q, want %q", reply.FileContent, fileContent))
	}
	if !reply.HasPass || reply.Passphrase != passphrase {
		mismatches = append(mismatches,
			fmt.Sprintf("passphrase: got %q, want %q", reply.Passphrase, passphrase))
	}
	switch {
	case first.EndedAt == nil:
		mismatches = append(mismatches,
			"first turn ended_at: missing, cannot bound the file modification time")
	case !reply.HasModTime:
		mismatches = append(mismatches,
			"file modification time: missing, want the stat -c %Y value")
	default:
		deadline := first.EndedAt.Add(suspendFileModTimeMargin)
		if got := time.Unix(reply.ModTime, 0).UTC(); got.After(deadline) {
			mismatches = append(mismatches, fmt.Sprintf(
				"file modification time: %s is later than the first turn ended_at %s plus %s",
				got.Format(time.RFC3339), first.EndedAt.UTC().Format(time.RFC3339), suspendFileModTimeMargin))
		}
	}
	return mismatches
}

func suspendRequireRunState(ctx context.Context, h *harness, runID, want string) {
	h.t.Helper()
	detail, err := h.getRun(ctx, runID)
	if err != nil {
		h.t.Fatalf("acceptance: read run %s after the restart: %v", runID, err)
	}
	if detail.State != want {
		h.t.Fatalf("acceptance: run %s is %q after the restart, want %q", runID, detail.State, want)
	}
}

// suspendRequireSuspensionHealth fails on the first suspension that recorded
// an error and requires the expected number of records. Checking after every
// cycle stops the scenario at the failed suspension instead of letting later
// turn assertions report a misleading mismatch.
func suspendRequireSuspensionHealth(t *testing.T, records []suspension, want int) {
	t.Helper()
	if len(records) != want {
		t.Fatalf("acceptance: run has %d suspension records, want %d", len(records), want)
	}
	for i, record := range records {
		if record.Error != "" {
			t.Fatalf("acceptance: suspension %d failed: %q", i+1, record.Error)
		}
	}
}

func suspendCheckSuspensionComplete(t *testing.T, records []suspension, want int) {
	t.Helper()
	if len(records) != want {
		t.Fatalf("acceptance: run has %d suspension records, want %d", len(records), want)
	}
	for i, record := range records {
		if record.Error != "" {
			t.Fatalf("acceptance: suspension %d has error %q", i+1, record.Error)
		}
		switch {
		case record.SnapshotMS == nil:
			t.Fatalf("acceptance: suspension %d has no snapshot_ms", i+1)
		case record.RestoreMS == nil:
			t.Fatalf("acceptance: suspension %d has no restore_ms", i+1)
		case record.HelloMS == nil:
			t.Fatalf("acceptance: suspension %d has no hello_ms", i+1)
		}
	}
}

// suspendCheckCLIGet checks the human-facing "run get" output: exactly one
// suspension and no dashed timing.
func suspendCheckCLIGet(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "suspensions: 1") {
		t.Fatalf("acceptance: run get output does not show exactly one suspension:\n%s", output)
	}
	for _, field := range []string{"snapshot_ms", "restore_ms", "hello_ms"} {
		if strings.Contains(output, field+" -") {
			t.Fatalf("acceptance: run get shows an empty %s:\n%s", field, output)
		}
	}
}

func suspendCounterInitialPrompt() string {
	return fmt.Sprintf(
		"在当前工作目录里创建文件 counter.txt，内容只有一行，恰好是数字 %d。只回复两行，按字面格式：第一行 count=<行数>，第二行 last=<最后一行>，例如 count=3、last=2。",
		suspendCounterInitial)
}

func suspendCounterCyclePrompt(k int) string {
	prompt := fmt.Sprintf(
		"先执行 date -u +%%s.%%N，把输出放在回复的第一行。然后往当前工作目录的 counter.txt 追加一行，内容只有数字 %d。之后另起两行，按字面格式：count=<行数> 和 last=<最后一行>，例如 count=3、last=2。",
		k)
	if k == suspendCycles {
		// paste -sd, puts the whole file on one line, so the answer survives
		// without depending on how a model formats a multi-line block.
		prompt += "最后执行 paste -sd, counter.txt，并把这条命令的输出按字面写成一行 file=<输出>；不要加代码块。"
	}
	return prompt
}

func suspendRequireCounterOutput(t *testing.T, observed turn, wantCount, wantLast int) {
	t.Helper()
	if observed.IsError {
		t.Fatalf("acceptance: turn is an error: %q", observed.Output)
	}
	count, last, ok := suspendParseCountLast(observed.Output)
	if !ok {
		t.Fatalf("acceptance: turn output %q has no count=/last= lines", observed.Output)
	}
	if count != wantCount || last != strconv.Itoa(wantLast) {
		t.Fatalf("acceptance: turn output says count=%d last=%q, want count=%d last=%d\n%s",
			count, last, wantCount, wantLast, observed.Output)
	}
}

// suspendParseCountLast reads the count= and last= markers. Later occurrences
// win, so an early echo of the instruction cannot override the actual answer.
// last keeps only the digits, which tolerates backticks, quotes and spaces
// around the number.
func suspendParseCountLast(output string) (count int, last string, ok bool) {
	countOK, lastOK := false, false
	for _, line := range strings.Split(output, "\n") {
		if match := suspendCountPattern.FindStringSubmatch(line); match != nil {
			value, err := strconv.Atoi(match[1])
			if err == nil {
				count = value
				countOK = true
			}
		}
		if match := suspendLastPattern.FindStringSubmatch(line); match != nil {
			last = match[1]
			lastOK = true
		}
	}
	return count, last, countOK && lastOK
}

// suspendParseGuestTime finds the epoch seconds on the first line of the
// reply. Values below 1e9 are skipped so an unrelated decimal in the prose
// cannot be mistaken for the guest clock.
func suspendParseGuestTime(output string) (time.Time, bool) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		match := suspendGuestTimePattern.FindString(line)
		if match == "" {
			continue
		}
		seconds, err := strconv.ParseFloat(match, 64)
		if err != nil || seconds < 1e9 {
			continue
		}
		whole := int64(seconds)
		nanos := int64((seconds - float64(whole)) * 1e9)
		return time.Unix(whole, nanos).UTC(), true
	}
	return time.Time{}, false
}

func suspendClockSkewFor(guest time.Time, observed turn) suspendClockSkew {
	ended := guest
	if observed.EndedAt != nil {
		ended = *observed.EndedAt
	}
	return suspendClockSkew{
		GuestTime:     guest,
		TurnStartedAt: observed.StartedAt,
		TurnEndedAt:   ended,
		MinMS:         guest.Sub(ended).Milliseconds(),
		MaxMS:         guest.Sub(observed.StartedAt).Milliseconds(),
	}
}

// suspendRequireCounterFile checks the single file= line produced by
// `paste -sd, counter.txt`: the initial line 0 followed by 1 to 10, each
// exactly once.
func suspendRequireCounterFile(t *testing.T, output string, m *suspendScenarioMeasurement) {
	t.Helper()
	values, ok := suspendParseCounterFileCSV(output)
	if !ok {
		t.Fatalf("acceptance: last turn output %q has no file= line", output)
	}
	want := suspendCycles + 1
	if len(values) != want {
		t.Fatalf("acceptance: counter.txt has %d lines, want %d: %q", len(values), want, values)
	}
	for i := 0; i < want; i++ {
		if values[i] != strconv.Itoa(i) {
			t.Fatalf("acceptance: counter.txt line %d is %q, want %d", i+1, values[i], i)
		}
	}
	m.addNote("counter.txt paste -sd, output: %s", strings.Join(values, ","))
}

// suspendParseCounterFileCSV extracts the last file= answer and splits the
// comma-separated values. It accepts an inline answer, inline code spans, and
// a fenced block whose opening fence carries a language name. Only the
// leading digit list is taken, so trailing prose does not become part of the
// answer.
func suspendParseCounterFileCSV(output string) ([]string, bool) {
	lines := strings.Split(output, "\n")
	start, inline := -1, ""
	for i, line := range lines {
		if match := suspendFilePattern.FindStringSubmatch(line); match != nil {
			start = i
			inline = strings.TrimSpace(match[1])
		}
	}
	if start < 0 {
		return nil, false
	}

	if values, ok := suspendParseCounterValues(inline); ok {
		return values, true
	}

	// The value may follow the marker in a fenced block or on the next line.
	fence := strings.HasPrefix(inline, "```")
	var collected []string
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			if len(collected) == 0 {
				fence = true
				continue
			}
			break
		}
		if !fence && len(collected) > 0 {
			break
		}
		collected = append(collected, trimmed)
	}
	if values, ok := suspendParseCounterValues(strings.Join(collected, ",")); ok {
		return values, true
	}
	return nil, false
}

// suspendParseCounterValues reads the leading comma-separated digit list,
// accepting the ASCII comma, the full-width comma, and optional decorations
// such as asterisks, backticks and quotes around the value.
func suspendParseCounterValues(value string) ([]string, bool) {
	match := suspendCounterValuePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return nil, false
	}
	fields := strings.FieldsFunc(match[1], func(r rune) bool { return r == ',' || r == '，' })
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			values = append(values, field)
		}
	}
	if len(values) == 0 {
		return nil, false
	}
	return values, true
}

// suspendWorkerAllocated sums the resources every worker in the pool reports
// as allocated. The pool is dedicated to this run, so the total rises by the
// actor template's CPU and memory while the actor runs and returns to the
// baseline while it is suspended.
func suspendWorkerAllocated(ctx context.Context, h *harness) (substrateWorkerResources, error) {
	out, err := kubectlAte(ctx, h.kubeCtx, "get", "workers", "-n", suspendWorkerNamespace, "-o", "json")
	if err != nil {
		return substrateWorkerResources{}, err
	}
	var list substrateWorkerList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return substrateWorkerResources{}, fmt.Errorf("decoding workers in %s: %w", suspendWorkerNamespace, err)
	}
	var total substrateWorkerResources
	for _, worker := range list.Workers {
		total.CPUMillicores += worker.Status.Allocated.CPUMillicores
		total.MemoryBytes += worker.Status.Allocated.MemoryBytes
	}
	return total, nil
}

func suspendAllocatedEqual(a, b substrateWorkerResources) bool {
	return a.CPUMillicores == b.CPUMillicores && a.MemoryBytes == b.MemoryBytes
}

func suspendAllocatedAbove(current, baseline substrateWorkerResources) bool {
	return current.CPUMillicores > baseline.CPUMillicores && current.MemoryBytes > baseline.MemoryBytes
}

// suspendAwaitAllocatedEqual waits until the pool reports its baseline again.
// The release can lag the SUSPENDED state slightly, so polling avoids a race
// that a single read would turn into a false failure.
func suspendAwaitAllocatedEqual(ctx context.Context, h *harness, want substrateWorkerResources) substrateWorkerResources {
	h.t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, suspendStateTimeout)
	defer cancel()
	var last substrateWorkerResources
	var lastErr error
	err := waitFor(waitCtx, "worker allocation to return to the baseline", func() (bool, error) {
		current, err := suspendWorkerAllocated(waitCtx, h)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last = current
		return suspendAllocatedEqual(current, want), nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v; baseline cpu=%d memory=%d, last cpu=%d memory=%d, last error: %v",
			err, want.CPUMillicores, want.MemoryBytes, last.CPUMillicores, last.MemoryBytes, lastErr)
	}
	return last
}

// suspendAwaitAllocatedAbove waits until a resumed actor occupies the pool
// again: both CPU and memory must rise above the baseline, because the actor
// template requests both.
func suspendAwaitAllocatedAbove(ctx context.Context, h *harness, baseline substrateWorkerResources) substrateWorkerResources {
	h.t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, suspendStateTimeout)
	defer cancel()
	var last substrateWorkerResources
	var lastErr error
	err := waitFor(waitCtx, "worker allocation to rise above the baseline after resume", func() (bool, error) {
		current, err := suspendWorkerAllocated(waitCtx, h)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last = current
		return suspendAllocatedAbove(current, baseline), nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v; baseline cpu=%d memory=%d, last cpu=%d memory=%d, last error: %v",
			err, baseline.CPUMillicores, baseline.MemoryBytes, last.CPUMillicores, last.MemoryBytes, lastErr)
	}
	return last
}

// suspendRunStateWatcher samples the run while a message wakes it, so
// SentWhileSuspended is backed by an observed resuming state instead of being
// assumed.
type suspendRunStateWatcher struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu           sync.Mutex
	resumingSeen bool
}

func startSuspendRunStateWatcher(parent context.Context, h *harness, runID string) *suspendRunStateWatcher {
	ctx, cancel := context.WithCancel(parent)
	w := &suspendRunStateWatcher{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(suspendResumePollInterval)
		defer ticker.Stop()
		for {
			if detail, err := h.getRun(ctx, runID); err == nil && detail.State == suspendStateResuming {
				w.mu.Lock()
				w.resumingSeen = true
				w.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return w
}

func (w *suspendRunStateWatcher) stop() {
	w.stopOnce.Do(func() {
		w.cancel()
		<-w.done
	})
}

func (w *suspendRunStateWatcher) sawResuming() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resumingSeen
}

// suspendMeasureAndAttachSnapshot measures the actor's snapshot for one
// suspension. A measurement that cannot be taken is a note, not a failure;
// the snapshot size is supporting evidence, while a suspension record with an
// error is what stops the scenario.
func suspendMeasureAndAttachSnapshot(ctx context.Context, h *harness, m *suspendScenarioMeasurement, tracker *suspendSnapshotTracker, index int, actor string) {
	h.t.Helper()
	measurements, err := suspendMeasureSnapshots(ctx, h, actor)
	if err != nil {
		m.addNote("suspension %d: snapshot size unmeasured: %v", index+1, err)
		return
	}
	measurement, ok := tracker.takeNewest(measurements)
	if !ok {
		m.addNote("suspension %d: status.snapshots has no new entry for this generation (%d entries); snapshot size unmeasured",
			index+1, len(measurements))
		return
	}
	m.attachSnapshot(index, measurement)
}

// TestSuspendParsesReplies covers the reply shapes the model returns for the
// counter protocol. The last value keeps only digits, and the file contents
// come from one paste -sd, line, optionally inside a fenced code block whose
// opening fence names a language.
func TestSuspendParsesReplies(t *testing.T) {
	t.Run("count and last", func(t *testing.T) {
		cases := []struct {
			name      string
			output    string
			wantCount int
			wantLast  string
			wantOK    bool
		}{
			{name: "literal", output: "count=3\nlast=2", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "spaces", output: "count = 3\nlast = 2", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "colons", output: "count: 3\nlast: 2", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "backticks", output: "count=11\nlast=`10`", wantCount: 11, wantLast: "10", wantOK: true},
			{name: "quotes", output: "count=11\nlast=\"10\"", wantCount: 11, wantLast: "10", wantOK: true},
			{name: "bold", output: "**count**：**3**\n**last**：**2**", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "full-width separators", output: "count＝3\nlast＝2", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "surrounding prose", output: "统计结果：count=3，last=2。", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "fenced with language", output: "```text\ncount=11\nlast=10\n```", wantCount: 11, wantLast: "10", wantOK: true},
			{name: "later answer wins", output: "count=1\nlast=0\ncount=3\nlast=2", wantCount: 3, wantLast: "2", wantOK: true},
			{name: "last without digits", output: "count=3\nlast=unknown", wantCount: 3, wantLast: "", wantOK: false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				count, last, ok := suspendParseCountLast(tc.output)
				if ok != tc.wantOK {
					t.Fatalf("ok = %t, want %t (count=%d last=%q)", ok, tc.wantOK, count, last)
				}
				if !tc.wantOK {
					return
				}
				if count != tc.wantCount || last != tc.wantLast {
					t.Fatalf("count=%d last=%q, want count=%d last=%q", count, last, tc.wantCount, tc.wantLast)
				}
			})
		}
	})

	t.Run("file csv", func(t *testing.T) {
		cases := []struct {
			name   string
			output string
			want   []string
			wantOK bool
		}{
			{name: "plain", output: "file=0,1,2,3", want: []string{"0", "1", "2", "3"}, wantOK: true},
			{name: "spaces", output: "file = 0, 1, 2", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "bold", output: "**file**：**0,1,2**", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "full-width separators", output: "file：0，1，2", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "trailing prose", output: "文件内容是 file=0,1,2，以上是全部内容。", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "leading prose", output: "执行结果 file=0,1,2", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "inline code", output: "file=`0,1,2`", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "fenced block", output: "```text\nfile=0,1,2\n```", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "fence with language", output: "file=\n```text\n0,1,2\n```", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "fence on the file line", output: "file=```text\n0,1,2\n```", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "plain on the next line", output: "file=\n0,1,2", want: []string{"0", "1", "2"}, wantOK: true},
			{name: "later file wins", output: "file=0,1\nfile=2,3", want: []string{"2", "3"}, wantOK: true},
			{name: "missing marker", output: "0,1,2", wantOK: false},
			{name: "empty value", output: "file=", wantOK: false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, ok := suspendParseCounterFileCSV(tc.output)
				if ok != tc.wantOK {
					t.Fatalf("ok = %t, want %t (got %v)", ok, tc.wantOK, got)
				}
				if !tc.wantOK {
					return
				}
				if len(got) != len(tc.want) {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("got %v, want %v", got, tc.want)
					}
				}
			})
		}
	})

	t.Run("file proof labels", func(t *testing.T) {
		cases := []struct {
			name      string
			output    string
			wantFile  string
			wantPass  string
			wantMtime int64
		}{
			{
				name:      "literal",
				output:    "file=edeaaff3f1774ad2\npass=pass-abc\nmtime=1700000000",
				wantFile:  "edeaaff3f1774ad2",
				wantPass:  "pass-abc",
				wantMtime: 1700000000,
			},
			{
				name:      "bold and full-width",
				output:    "**file**：**edeaaff3f1774ad2**\n**pass**：**pass-abc**\n**mtime**：**1700000000**",
				wantFile:  "edeaaff3f1774ad2",
				wantPass:  "pass-abc",
				wantMtime: 1700000000,
			},
			{
				name:      "surrounding prose",
				output:    "文件内容 file=edeaaff3f1774ad2，暗号 pass：pass-abc，修改时间 mtime＝1700000000 秒",
				wantFile:  "edeaaff3f1774ad2",
				wantPass:  "pass-abc",
				wantMtime: 1700000000,
			},
			{
				name:      "later answer wins",
				output:    "file=deadbeef\npass=pass-old\nmtime=1\nfile=edeaaff3f1774ad2\npass=pass-abc\nmtime=1700000000",
				wantFile:  "edeaaff3f1774ad2",
				wantPass:  "pass-abc",
				wantMtime: 1700000000,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				proof := suspendParseFileProof(tc.output)
				if proof.FileContent != tc.wantFile {
					t.Errorf("file content = %q, want %q", proof.FileContent, tc.wantFile)
				}
				if proof.Passphrase != tc.wantPass {
					t.Errorf("passphrase = %q, want %q", proof.Passphrase, tc.wantPass)
				}
				if !proof.HasModTime || proof.ModTime != tc.wantMtime {
					t.Errorf("mtime = %d (present %t), want %d", proof.ModTime, proof.HasModTime, tc.wantMtime)
				}
			})
		}
	})
}

// TestSuspendFileProof checks the prompt structure and the expected file
// content. The recall prompt must not name the file; only the session can
// supply it.
func TestSuspendFileProof(t *testing.T) {
	if got, want := suspendFileContent("abc"), "edeaaff3f1774ad2"; got != want {
		t.Fatalf("suspendFileContent(abc) = %q, want %q", got, want)
	}

	first := suspendFileProofPrompt("marker-1", "pass-1")
	for _, want := range []string{"echo marker-1 | sha256sum | cut -c1-16 > result.txt", "pass-1", "不要回显"} {
		if !strings.Contains(first, want) {
			t.Errorf("first prompt does not contain %q:\n%s", want, first)
		}
	}

	recall := suspendFileRecallPrompt()
	if strings.Contains(recall, "result.txt") {
		t.Errorf("recall prompt names the file:\n%s", recall)
	}
	if !strings.Contains(recall, "刚才") {
		t.Errorf("recall prompt does not refer to the file just created:\n%s", recall)
	}
	for _, want := range []string{"stat -c %Y", "不要重新生成或修改这个文件", "只读取", "mtime"} {
		if !strings.Contains(recall, want) {
			t.Errorf("recall prompt does not contain %q:\n%s", want, recall)
		}
	}
}

// TestSuspendFileProofMtime checks that the second turn proves the file was
// not recreated: its modification time must not be later than the first
// turn's ended_at plus a small margin, and every failed check is reported
// together instead of stopping at the first one.
func TestSuspendFileProofMtime(t *testing.T) {
	content := "edeaaff3f1774ad2"
	passphrase := "pass-abc"
	firstEnded := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	first := turn{EndedAt: &firstEnded}
	reply := func(file, pass string, mtime int64) turn {
		return turn{Output: fmt.Sprintf("file=%s\npass=%s\nmtime=%d", file, pass, mtime)}
	}

	t.Run("within the margin", func(t *testing.T) {
		observed := reply(content, passphrase, firstEnded.Add(suspendFileModTimeMargin).Unix())
		if got := suspendFileProofMismatches(observed, first, content, passphrase); len(got) != 0 {
			t.Fatalf("mismatches = %v, want none", got)
		}
	})

	t.Run("later than the first turn", func(t *testing.T) {
		observed := reply(content, passphrase, firstEnded.Add(suspendFileModTimeMargin+time.Second).Unix())
		got := suspendFileProofMismatches(observed, first, content, passphrase)
		if len(got) != 1 || !strings.Contains(got[0], "modification time") {
			t.Fatalf("mismatches = %v, want exactly one modification-time mismatch", got)
		}
	})

	t.Run("missing ended_at", func(t *testing.T) {
		observed := reply(content, passphrase, firstEnded.Unix())
		got := suspendFileProofMismatches(observed, turn{}, content, passphrase)
		if len(got) != 1 || !strings.Contains(got[0], "ended_at") {
			t.Fatalf("mismatches = %v, want one ended_at mismatch", got)
		}
	})

	t.Run("all mismatches reported", func(t *testing.T) {
		observed := reply("deadbeef", "pass-wrong", firstEnded.Add(time.Hour).Unix())
		got := suspendFileProofMismatches(observed, first, content, passphrase)
		if len(got) != 3 {
			t.Fatalf("mismatches = %v, want file, passphrase and modification time", got)
		}
	})
}

// TestSuspendProvenanceUsesActorCommit checks that RepoCommit is the commit
// passed in for the image, not the repository HEAD the test runs from.
func TestSuspendProvenanceUsesActorCommit(t *testing.T) {
	m := newSuspendScenario("provenance")
	suspendRecordProvenance(m, "registry.example/actor@sha256:abc", "430f63a")
	if m.RepoCommit != "430f63a" {
		t.Fatalf("RepoCommit = %q, want 430f63a", m.RepoCommit)
	}
	if m.ActorImage != "registry.example/actor@sha256:abc" {
		t.Fatalf("ActorImage = %q", m.ActorImage)
	}

	missing := newSuspendScenario("missing")
	suspendRecordProvenance(missing, "image", "")
	if missing.RepoCommit != "" {
		t.Fatalf("RepoCommit = %q, want empty", missing.RepoCommit)
	}
	if len(missing.Notes) == 0 {
		t.Fatal("a missing actor commit produced no note")
	}
}

// TestSuspendAllocatedComparison covers the capacity-release comparisons.
func TestSuspendAllocatedComparison(t *testing.T) {
	baseline := substrateWorkerResources{CPUMillicores: 100, MemoryBytes: 1 << 30}
	if !suspendAllocatedEqual(baseline, baseline) {
		t.Error("identical allocations are not equal")
	}
	if suspendAllocatedEqual(baseline, substrateWorkerResources{CPUMillicores: 101, MemoryBytes: 1 << 30}) {
		t.Error("different CPU allocations compare equal")
	}
	if suspendAllocatedAbove(baseline, baseline) {
		t.Error("the baseline is above itself")
	}
	resumed := substrateWorkerResources{CPUMillicores: 2100, MemoryBytes: 3 << 30}
	if !suspendAllocatedAbove(resumed, baseline) {
		t.Errorf("%+v is not above %+v", resumed, baseline)
	}
	if suspendAllocatedAbove(substrateWorkerResources{CPUMillicores: 2100, MemoryBytes: 1 << 30}, baseline) {
		t.Error("a CPU-only increase must not count as the actor occupying the pool again")
	}
}

// TestSuspendRunStateWatcher checks that SentWhileSuspended has evidence: the
// watcher records the resuming state when it appears and stays false when the
// run never reports it.
func TestSuspendRunStateWatcher(t *testing.T) {
	runWatcher := func(t *testing.T, states ...string) *suspendRunStateWatcher {
		t.Helper()
		var mu sync.Mutex
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			index := calls
			calls++
			mu.Unlock()
			if index >= len(states) {
				index = len(states) - 1
			}
			_, _ = fmt.Fprintf(w, `{"run_id":"run-1","state":%q}`, states[index])
		}))
		t.Cleanup(server.Close)

		h := &harness{
			t:        t,
			httpAddr: strings.TrimPrefix(server.URL, "http://"),
			client:   server.Client(),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		return startSuspendRunStateWatcher(ctx, h, "run-1")
	}

	t.Run("observes resuming", func(t *testing.T) {
		w := runWatcher(t, suspendStateSuspended, suspendStateResuming, "active")
		deadline := time.Now().Add(2 * time.Second)
		for !w.sawResuming() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		w.stop()
		if !w.sawResuming() {
			t.Fatal("the watcher did not observe resuming")
		}
	})

	t.Run("stays false without resuming", func(t *testing.T) {
		w := runWatcher(t, suspendStateSuspended, "active")
		time.Sleep(200 * time.Millisecond)
		w.stop()
		if w.sawResuming() {
			t.Fatal("the watcher reported resuming although the run never entered it")
		}
	})
}

// TestMeasureSnapshotTracker covers the per-cycle snapshot selection: only
// the URI that was not seen before is attached, and an unmeasured snapshot is
// recorded as a note instead of being reported as zero bytes.
func TestMeasureSnapshotTracker(t *testing.T) {
	m := newSuspendScenario("tracker")
	m.setSuspensions(runDetail{Suspensions: []suspension{{}, {}}})
	tracker := newSuspendSnapshotTracker()

	first, ok := tracker.takeNewest([]snapshotMeasurement{
		{URI: "gs://bucket/1", Measured: true, Bytes: 10},
		{URI: "gs://bucket/2", Measured: true, Bytes: 20},
	})
	if !ok || first.URI != "gs://bucket/2" {
		t.Fatalf("first takeNewest = %+v, %t; want the newest entry", first, ok)
	}
	m.attachSnapshot(0, first)

	second, ok := tracker.takeNewest([]snapshotMeasurement{
		{URI: "gs://bucket/2", Measured: true, Bytes: 20},
		{URI: "gs://bucket/3", Measured: true, Bytes: 30},
	})
	if !ok || second.URI != "gs://bucket/3" {
		t.Fatalf("second takeNewest = %+v, %t; want the newly added entry", second, ok)
	}
	m.attachSnapshot(1, second)

	if len(m.SnapshotSizes) != 2 {
		t.Fatalf("SnapshotSizes has %d entries, want 2", len(m.SnapshotSizes))
	}
	if m.Suspensions[0].SnapshotSize == nil || m.Suspensions[0].SnapshotSize.Bytes != 20 {
		t.Errorf("suspension 1 size = %+v, want 20 bytes", m.Suspensions[0].SnapshotSize)
	}
	if m.Suspensions[1].SnapshotSize == nil || m.Suspensions[1].SnapshotSize.Bytes != 30 {
		t.Errorf("suspension 2 size = %+v, want 30 bytes", m.Suspensions[1].SnapshotSize)
	}
	if m.SnapshotSizeSummary == nil || m.SnapshotSizeSummary.Count != 2 {
		t.Errorf("summary = %+v, want count 2", m.SnapshotSizeSummary)
	}

	if _, ok := tracker.takeNewest([]snapshotMeasurement{{URI: "gs://bucket/3", Measured: true, Bytes: 30}}); ok {
		t.Error("takeNewest accepted an already-seen URI as new")
	}

	unmeasured := newSuspendScenario("unmeasured")
	unmeasured.setSuspensions(runDetail{Suspensions: []suspension{{}}})
	unmeasuredTracker := newSuspendSnapshotTracker()
	measurement, ok := unmeasuredTracker.takeNewest([]snapshotMeasurement{{URI: "gs://bucket/x", Error: "du failed"}})
	if !ok {
		t.Fatal("takeNewest rejected the only snapshot entry")
	}
	unmeasured.attachSnapshot(0, measurement)
	if unmeasured.Suspensions[0].SnapshotSize != nil {
		t.Errorf("unmeasured snapshot set SnapshotSize = %+v", unmeasured.Suspensions[0].SnapshotSize)
	}
	if !strings.Contains(unmeasured.Suspensions[0].SnapshotNote, "du failed") {
		t.Errorf("SnapshotNote = %q, want the measurement error", unmeasured.Suspensions[0].SnapshotNote)
	}
}
