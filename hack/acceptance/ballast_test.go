//go:build acceptance

// Memory-ballast suspend/resume acceptance test.
//
// The go criterion measures snapshot restore with an actor holding about
// 500 MB of written memory. TestBallastSuspendResume provisions one actor from
// the ballast image in hack/acceptance/ballast and runs five snapshot/restore
// cycles directly through internal/sandboxprovider/substrate, without a
// control plane and without spending LLM quota. Each cycle records the
// snapshot and restore durations, the snapshot size, the snapshot generation
// and the resident set size; after every restore the same process must still
// run and the report must confirm that every page still holds the content its
// page number implies.
//
// The actor reports through the same egress path the warden uses. The ballast
// image POSTs to castworks-dev.castworks-system.svc.cluster.local:9090 over
// cleartext HTTP/2 with prior knowledge, and the EgressPolicy allows that name
// and port with an http rule. The test serves those reports on the development
// host's address behind the selectorless Service, so nothing is port-forwarded
// from ate-system. The report server binds 10.12.72.11:9090 for the duration of
// the test, which makes this test mutually exclusive with a control plane on
// the development host, with the suspend/resume acceptance run and with the
// Substrate provider integration tests: they all use the same address, and the
// test fails immediately when it cannot bind it.
//
// The test needs the ballast image pinned by digest, in the
// name@sha256:<digest> form build.sh prints:
//
//	CASTWORKS_ACC_BALLAST_IMAGE=<repo>@sha256:<digest> \
//	  go test -tags acceptance -count=1 -v \
//	  -run TestBallastSuspendResume ./hack/acceptance/
//
// The cluster context comes from CASTWORKS_ACC_KUBE_CONTEXT when it is set,
// like the other acceptance tests. Results are written to
// ballast_measurements.json in the per-test working directory below
// /tmp/castworks-acceptance/<UTC timestamp>/; the file is separate from the
// measurements.json of TestSuspendResume so the two tests cannot overwrite
// each other's samples. The actor is recorded in the ledger before Provision,
// so a killed run leaves a record that acceptance-clean can remove.
package acceptance

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/sandboxprovider"
	"github.com/wangzw/castworks/internal/sandboxprovider/substrate"
)

const (
	// ballastImageEnv holds the linux/arm64 manifest digest of the ballast
	// image, in the name@sha256:<digest> form that hack/acceptance/ballast/
	// build.sh prints as its last line.
	ballastImageEnv = "CASTWORKS_ACC_BALLAST_IMAGE"

	// The template every actor shares is keyed by the image digest. The
	// ballast reads CASTWORKS_CONTROL_PLANE as its report target; the value is
	// the same FQDN the production template uses (docs/milestone/v0.1.0/
	// sandboxprovider.md "actor 模板"), and the EgressPolicy allows it with an
	// http rule.
	ballastControlPlane   = "castworks-dev.castworks-system.svc.cluster.local:9090"
	ballastServiceAccount = "castworks-system/sandboxprovider"

	// ballastCycles is the number of snapshot/restore cycles the work item
	// asks for.
	ballastCycles = 5

	// ballastExpectedMiB mirrors ballastMiB in hack/acceptance/ballast/main.go.
	// A report is checked against it so a ballast image built with a different
	// size fails loudly instead of being measured as if it were the
	// criterion's 500 MB actor.
	ballastExpectedMiB   = 500
	ballastExpectedBytes = ballastExpectedMiB << 20
	ballastPageSize      = 4096
	ballastExpectedPages = ballastExpectedBytes / ballastPageSize

	ballastTimeout          = 45 * time.Minute
	ballastCallTimeout      = 10 * time.Minute
	ballastReportTimeout    = 3 * time.Minute
	ballastSnapshotTimeout  = 2 * time.Minute
	ballastQuiet            = 1500 * time.Millisecond
	ballastQuietMax         = 10 * time.Second
	ballastReportBodyLimit  = 1 << 20
	ballastRecordName       = "ballast_measurements.json"
	ballastReportPath       = "/report"
	ballastP95Definition    = "nearest rank: rank = ceil(0.95*n), one-based, so with 5 samples p95 equals the maximum"
	ballastReportServerNote = "the control plane, the suspend acceptance run and the provider integration tests use the same address and must not run at the same time"
)

// TestBallastSuspendResume provisions the ballast actor, then snapshots and
// restores it five times. Each restore must leave the same process alive with
// the same memory contents, as proved by the reports it posts through the
// egress gateway.
func TestBallastSuspendResume(t *testing.T) {
	ctx := testContext(t, ballastTimeout)

	image := requireEnv(t, ballastImageEnv)
	if !actorImageRef.MatchString(image) {
		t.Fatalf("acceptance: %s is %q, want name@sha256:<64 hex digits>", ballastImageEnv, image)
	}
	kubeContext := optionalEnv(envKubeContext)

	// Binding the report address before anything starts is also the
	// precondition check: a control plane that already listens there means
	// another acceptance run is in progress and the two must not overlap.
	hub := startBallastReportServer(t, substrateGRPCAddr)

	provider, err := substrate.New(ctx, substrate.Config{
		KubeContext:    kubeContext,
		Atespace:       ateSpace,
		Image:          image,
		ControlPlane:   ballastControlPlane,
		ServiceAccount: ballastServiceAccount,
	})
	if err != nil {
		t.Fatalf("acceptance: create substrate provider: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Close(); err != nil {
			t.Errorf("acceptance: close substrate provider: %v", err)
		}
	})

	runID, err := ballastRunID()
	if err != nil {
		t.Fatalf("acceptance: generate run ID: %v", err)
	}
	actor := sandboxprovider.ActorName(runID)

	reports, unsubscribe := hub.subscribe(actor)
	t.Cleanup(unsubscribe)

	// Destroy is registered before Provision because a failed Provision can
	// still have created the actor; Destroy on a missing actor is a no-op.
	t.Cleanup(func() { destroyBallastActor(t, provider, kubeContext, actor) })

	workDir := ballastWorkDir(t)

	// Record the actor before Provision so acceptance-clean can remove it when
	// this process is killed before t.Cleanup runs.
	if err := appendLedger(ledgerEntry{
		Time:     time.Now().UTC(),
		RunID:    runID,
		WorkDir:  workDir,
		Provider: "substrate",
	}); err != nil {
		t.Fatalf("acceptance: record actor %s in ledger: %v", actor, err)
	}

	provisionCtx, cancelProvision := context.WithTimeout(ctx, ballastCallTimeout)
	provision, err := provider.Provision(provisionCtx, runID)
	cancelProvision()
	if err != nil {
		t.Fatalf("acceptance: provision actor %s: %v", actor, err)
	}
	t.Logf("ballast actor %s provisioned in %s", actor, provision.Duration)

	first := waitForBallastReport(t, reports, ballastReportTimeout, func(rep ballastReport) bool {
		return rep.ActorName == actor
	})
	if err := checkBallastReport(first, actor, "", ""); err != nil {
		t.Fatalf("acceptance: first report: %v", err)
	}
	t.Logf("ballast ready: bytes=%d rss=%d crc32=%s started_at=%s pid=%d",
		first.BallastBytes, first.RSSBytes, first.CRC32, first.StartedAt, first.PID)

	record := ballastRecord{
		Actor:            actor,
		RunID:            runID,
		Image:            image,
		PID:              first.PID,
		BallastBytes:     first.BallastBytes,
		BallastCRC32:     first.CRC32,
		BallastStartedAt: first.StartedAt,
		ProvisionMS:      provision.Duration.Milliseconds(),
		RSSBytes:         first.RSSBytes,
		P95Definition:    ballastP95Definition,
	}
	recordPath := filepath.Join(workDir, ballastRecordName)
	t.Cleanup(func() {
		record.summarize()
		if err := writeBallastRecord(recordPath, record); err != nil {
			t.Errorf("acceptance: write %s: %v", recordPath, err)
		}
	})

	sizer := newDefaultSnapshotSizer()
	var previousGeneration int64
	for cycle := 1; cycle <= ballastCycles; cycle++ {
		snapshotCtx, cancelSnapshot := context.WithTimeout(ctx, ballastCallTimeout)
		snapshot, err := provider.Snapshot(snapshotCtx, actor)
		cancelSnapshot()
		if err != nil {
			t.Fatalf("acceptance: cycle %d: snapshot actor %s: %v", cycle, actor, err)
		}

		// The snapshot has returned and the process is frozen. Consume the
		// reports received before the freeze; the last one is the state the
		// restore must preserve, and its sequence is the threshold a report
		// after the restore must exceed.
		before, found := lastBallastReportAfterQuiet(reports, ballastQuiet, ballastQuietMax)
		if !found {
			before = first
		}

		// The status can trail the synchronous suspend by a moment. Requiring a
		// generation above the previous cycle's stops the test from measuring
		// the previous snapshot again when that lag happens.
		snapshotRef, err := waitBallastSnapshotAfter(ctx, kubeContext, actor, previousGeneration, ballastSnapshotTimeout)
		if err != nil {
			t.Fatalf("acceptance: cycle %d: wait for a new snapshot generation: %v", cycle, err)
		}

		size := sizer.measureURI(ctx, snapshotRef.URI)
		if !size.Measured {
			// Keep the round and the reason in the record before deciding to
			// fail: a measurement failure must not lose the samples collected
			// so far.
			record.Cycles = append(record.Cycles, ballastCycleMeasurement{
				Cycle:              cycle,
				SnapshotMS:         snapshot.Duration.Milliseconds(),
				SnapshotGeneration: snapshotRef.Generation,
				Snapshot:           size,
			})
			record.summarize()
			if err := writeBallastRecord(recordPath, record); err != nil {
				t.Errorf("acceptance: write %s: %v", recordPath, err)
			}
			t.Fatalf("acceptance: cycle %d: measure snapshot %s (generation %d): %s",
				cycle, snapshotRef.URI, snapshotRef.Generation, size.Error)
		}
		previousGeneration = snapshotRef.Generation

		restoreCtx, cancelRestore := context.WithTimeout(ctx, ballastCallTimeout)
		restore, err := provider.RestoreSnapshot(restoreCtx, actor)
		cancelRestore()
		if err != nil {
			t.Fatalf("acceptance: cycle %d: restore actor %s: %v", cycle, actor, err)
		}
		restoredAt := time.Now()

		after := waitForBallastReport(t, reports, ballastReportTimeout, func(rep ballastReport) bool {
			return rep.ActorName == actor && rep.Sequence > before.Sequence
		})
		readyMS := time.Since(restoredAt).Milliseconds()

		verify := after.Verify
		measurement := ballastCycleMeasurement{
			Cycle:              cycle,
			SnapshotMS:         snapshot.Duration.Milliseconds(),
			RestoreMS:          restore.Duration.Milliseconds(),
			ReadyMS:            readyMS,
			VerifyScanMS:       after.Verify.ScanMS,
			SnapshotGeneration: snapshotRef.Generation,
			RSSBytes:           after.RSSBytes,
			Snapshot:           size,
			Verify:             &verify,
		}
		record.Cycles = append(record.Cycles, measurement)

		if err := checkBallastReport(after, actor, record.BallastCRC32, record.BallastStartedAt); err != nil {
			t.Fatalf("acceptance: cycle %d: report after restore: %v", cycle, err)
		}
		if after.PID != record.PID {
			t.Fatalf("acceptance: cycle %d: report pid = %d, want %d; the process was replaced rather than restored",
				cycle, after.PID, record.PID)
		}
		t.Logf("ballast cycle %d: snapshot=%s restore=%s ready=%dms bytes=%d generation=%d verify_scan=%dms rss=%d",
			cycle, snapshot.Duration, restore.Duration, readyMS, size.Bytes,
			snapshotRef.Generation, after.Verify.ScanMS, after.RSSBytes)
	}

	destroyCtx, cancelDestroy := context.WithTimeout(ctx, 3*time.Minute)
	err = provider.Destroy(destroyCtx, actor)
	if err == nil {
		err = waitActorGone(destroyCtx, kubeContext, actor, actorGoneTimeout)
	}
	cancelDestroy()
	if err != nil {
		t.Fatalf("acceptance: destroy actor %s: %v", actor, err)
	}
}

// ballastReport is the report body from hack/acceptance/ballast.
type ballastReport struct {
	ActorName    string                `json:"actor_name"`
	StartupID    string                `json:"startup_id"`
	StartedAt    string                `json:"started_at"`
	Sequence     int64                 `json:"sequence"`
	PID          int                   `json:"pid"`
	BallastBytes int                   `json:"ballast_bytes"`
	BallastMiB   int                   `json:"ballast_mib"`
	CRC32        string                `json:"crc32"`
	RSSBytes     int64                 `json:"rss_bytes"`
	RSSError     string                `json:"rss_error,omitempty"`
	ReportAt     string                `json:"report_at"`
	Verify       ballastVerifyResponse `json:"verify"`
}

// ballastVerifyResponse is the page-by-page verification result in a report.
type ballastVerifyResponse struct {
	Match               bool   `json:"match"`
	Pages               int    `json:"pages"`
	PageSize            int    `json:"page_size"`
	FirstMismatchPage   *int   `json:"first_mismatch_page,omitempty"`
	FirstMismatchOffset *int64 `json:"first_mismatch_offset_bytes,omitempty"`
	ScanMS              int64  `json:"scan_ms"`
}

// checkBallastReport verifies the fields that must not change across a
// snapshot and restore. Empty wantCRC and wantStartedAt mean the caller has no
// baseline yet.
func checkBallastReport(rep ballastReport, actor, wantCRC, wantStartedAt string) error {
	if rep.ActorName != actor {
		return fmt.Errorf("actor_name = %q, want %q", rep.ActorName, actor)
	}
	if rep.BallastBytes != ballastExpectedBytes || rep.BallastMiB != ballastExpectedMiB {
		return fmt.Errorf("ballast is %d bytes (%d MiB), want %d bytes (%d MiB)",
			rep.BallastBytes, rep.BallastMiB, ballastExpectedBytes, ballastExpectedMiB)
	}
	if rep.CRC32 == "" {
		return fmt.Errorf("report has no crc32")
	}
	if wantCRC != "" && rep.CRC32 != wantCRC {
		return fmt.Errorf("crc32 = %q, want %q", rep.CRC32, wantCRC)
	}
	if wantStartedAt != "" && rep.StartedAt != wantStartedAt {
		return fmt.Errorf("started_at = %q, want %q; the process was replaced rather than restored",
			rep.StartedAt, wantStartedAt)
	}
	if rep.StartupID == "" {
		return fmt.Errorf("report has no startup_id")
	}
	if rep.RSSError != "" || rep.RSSBytes <= 0 {
		return fmt.Errorf("rss = %d (%s), want a positive resident set size", rep.RSSBytes, rep.RSSError)
	}
	if rep.Verify.Pages != ballastExpectedPages || rep.Verify.PageSize != ballastPageSize {
		return fmt.Errorf("verify covers %d pages of %d bytes, want %d pages of %d bytes",
			rep.Verify.Pages, rep.Verify.PageSize, ballastExpectedPages, ballastPageSize)
	}
	if !rep.Verify.Match {
		return fmt.Errorf("restored ballast differs at page %s offset %s",
			ballastOptionalInt(rep.Verify.FirstMismatchPage), ballastOptionalInt64(rep.Verify.FirstMismatchOffset))
	}
	return nil
}

// ballastCycleMeasurement is one snapshot/restore cycle. Snapshot is the size
// measured from the rustfs local PV; the provider itself always reports zero.
// Verify is absent when the cycle stopped before the actor reported again.
type ballastCycleMeasurement struct {
	Cycle              int                    `json:"cycle"`
	SnapshotMS         int64                  `json:"snapshot_ms"`
	RestoreMS          int64                  `json:"restore_ms"`
	ReadyMS            int64                  `json:"ready_ms"`
	VerifyScanMS       int64                  `json:"verify_scan_ms"`
	SnapshotGeneration int64                  `json:"snapshot_generation"`
	RSSBytes           int64                  `json:"rss_bytes,omitempty"`
	Snapshot           snapshotMeasurement    `json:"snapshot"`
	Verify             *ballastVerifyResponse `json:"verify,omitempty"`
}

// ballastRecord is the memory-ballast measurement record. It is written even
// when the test fails so the samples collected up to the failure survive.
type ballastRecord struct {
	Actor            string                    `json:"actor"`
	RunID            string                    `json:"run_id"`
	Image            string                    `json:"image"`
	PID              int                       `json:"pid"`
	BallastBytes     int                       `json:"ballast_bytes"`
	BallastCRC32     string                    `json:"ballast_crc32"`
	BallastStartedAt string                    `json:"ballast_started_at"`
	ProvisionMS      int64                     `json:"provision_ms"`
	RSSBytes         int64                     `json:"rss_bytes"`
	P95Definition    string                    `json:"p95_definition"`
	Cycles           []ballastCycleMeasurement `json:"cycles"`
	SnapshotMS       ballastStats              `json:"snapshot_ms"`
	RestoreMS        ballastStats              `json:"restore_ms"`
	SnapshotBytes    ballastSizeStats          `json:"snapshot_bytes"`
}

// ballastStats summarizes one duration in milliseconds. Median and P95 use the
// nearest-rank definition; with five samples p95 equals the maximum.
type ballastStats struct {
	Count    int   `json:"count"`
	MedianMS int64 `json:"median_ms"`
	P95MS    int64 `json:"p95_ms"`
	MaxMS    int64 `json:"max_ms"`
}

// ballastSizeStats summarizes measured snapshot sizes. Unmeasured snapshots
// are counted separately and are not part of the median, p95 or maximum.
type ballastSizeStats struct {
	Count       int   `json:"count"`
	Unmeasured  int   `json:"unmeasured"`
	MedianBytes int64 `json:"median_bytes"`
	P95Bytes    int64 `json:"p95_bytes"`
	MaxBytes    int64 `json:"max_bytes"`
}

// summarize fills the aggregate fields from the collected cycles.
func (r *ballastRecord) summarize() {
	snapshotMS := make([]int64, 0, len(r.Cycles))
	restoreMS := make([]int64, 0, len(r.Cycles))
	sizes := make([]int64, 0, len(r.Cycles))
	unmeasured := 0
	for _, cycle := range r.Cycles {
		snapshotMS = append(snapshotMS, cycle.SnapshotMS)
		restoreMS = append(restoreMS, cycle.RestoreMS)
		if cycle.Snapshot.Measured {
			sizes = append(sizes, cycle.Snapshot.Bytes)
		} else {
			unmeasured++
		}
	}

	r.SnapshotMS = ballastDurationStats(snapshotMS)
	r.RestoreMS = ballastDurationStats(restoreMS)
	r.SnapshotBytes = ballastSnapshotSizeStats(sizes, unmeasured)
}

// ballastDurationStats computes the median, p95 and maximum of values. The
// input slice is not modified.
func ballastDurationStats(values []int64) ballastStats {
	if len(values) == 0 {
		return ballastStats{}
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return ballastStats{
		Count:    len(sorted),
		MedianMS: ballastMedian(sorted),
		P95MS:    ballastNearestRank(sorted, 0.95),
		MaxMS:    sorted[len(sorted)-1],
	}
}

// ballastSnapshotSizeStats computes the size summary over measured sizes.
func ballastSnapshotSizeStats(sizes []int64, unmeasured int) ballastSizeStats {
	stats := ballastSizeStats{Unmeasured: unmeasured}
	if len(sizes) == 0 {
		return stats
	}
	sorted := append([]int64(nil), sizes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	stats.Count = len(sorted)
	stats.MedianBytes = ballastMedian(sorted)
	stats.P95Bytes = ballastNearestRank(sorted, 0.95)
	stats.MaxBytes = sorted[len(sorted)-1]
	return stats
}

// ballastMedian returns the middle of an already sorted slice. For an even
// count it returns the integer mean of the two middle values.
func ballastMedian(sorted []int64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// ballastNearestRank returns the nearest-rank percentile of an already sorted
// slice: the value at rank ceil(percentile*n), one-based.
func ballastNearestRank(sorted []int64, percentile float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(percentile * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// ballastWorkDir returns this test's working directory below the run root.
func ballastWorkDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(runRoot(t), sanitizeTestName(t.Name()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("acceptance: create work directory %s: %v", dir, err)
	}
	return dir
}

// writeBallastRecord writes the measurement record as indented JSON.
func writeBallastRecord(path string, record ballastRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode measurement record: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write measurement record: %w", err)
	}
	return nil
}

// ballastRunID returns a random 12-character lowercase base32 run ID. The
// provider derives the actor name "cw-<runID>" from it, which must be a DNS
// label; this form satisfies that without depending on a control plane.
func ballastRunID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return strings.ToLower(encoded[:12]), nil
}

// ballastReportHub routes actor reports to the subscribers waiting for that
// actor.
type ballastReportHub struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[string]map[int]chan ballastReport
}

func newBallastReportHub() *ballastReportHub {
	return &ballastReportHub{subscribers: make(map[string]map[int]chan ballastReport)}
}

// subscribe returns a buffered stream of reports for one actor and a function
// that removes it. Reports are dropped rather than blocking the reporter when
// a subscriber falls behind.
func (h *ballastReportHub) subscribe(actor string) (<-chan ballastReport, func()) {
	ch := make(chan ballastReport, 1024)
	h.mu.Lock()
	if h.subscribers[actor] == nil {
		h.subscribers[actor] = make(map[int]chan ballastReport)
	}
	id := h.nextID
	h.nextID++
	h.subscribers[actor][id] = ch
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if subs := h.subscribers[actor]; subs != nil {
			delete(subs, id)
			if len(subs) == 0 {
				delete(h.subscribers, actor)
			}
		}
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

func (h *ballastReportHub) publish(rep ballastReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subscribers[rep.ActorName] {
		select {
		case ch <- rep:
		default:
		}
	}
}

func (h *ballastReportHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != ballastReportPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ballastReportBodyLimit))
	if err != nil {
		http.Error(w, "read report: "+err.Error(), http.StatusBadRequest)
		return
	}
	var rep ballastReport
	if err := json.Unmarshal(body, &rep); err != nil {
		http.Error(w, "decode report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rep.ActorName == "" {
		http.Error(w, "report has no actor_name", http.StatusBadRequest)
		return
	}
	h.publish(rep)
	w.WriteHeader(http.StatusNoContent)
}

// startBallastReportServer serves reports on addr, the development host's
// address behind the castworks-dev Service, over cleartext HTTP/2 with prior
// knowledge. The listener is opened before anything else starts so an address
// already in use fails the test immediately. Closing the server releases the
// address when the test ends.
func startBallastReportServer(t *testing.T, addr string) *ballastReportHub {
	t.Helper()

	hub := newBallastReportHub()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("acceptance: listen on %s for ballast reports: %v; %s", addr, err, ballastReportServerNote)
	}

	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Handler:           hub,
		Protocols:         &protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("acceptance: ballast report server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("acceptance: close ballast report server: %v", err)
		}
	})
	return hub
}

// waitForBallastReport waits for a report matching predicate.
func waitForBallastReport(t *testing.T, reports <-chan ballastReport, timeout time.Duration, match func(ballastReport) bool) ballastReport {
	t.Helper()
	deadline := time.After(timeout)
	var last ballastReport
	for {
		select {
		case rep := <-reports:
			last = rep
			if match(rep) {
				return rep
			}
		case <-deadline:
			t.Fatalf("no matching ballast report within %s (last report: %+v)", timeout, last)
		}
	}
}

// lastBallastReportAfterQuiet returns the last report received before a quiet
// gap of the given length, waiting at most max for one to arrive.
func lastBallastReportAfterQuiet(reports <-chan ballastReport, quiet, max time.Duration) (ballastReport, bool) {
	deadline := time.Now().Add(max)
	var last ballastReport
	found := false
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last, found
		}
		timer := time.NewTimer(min(remaining, quiet))
		select {
		case rep := <-reports:
			timer.Stop()
			last, found = rep, true
		case <-timer.C:
			if found {
				return last, true
			}
		}
	}
}

// waitBallastSnapshotAfter reads the actor's newest snapshot and requires its
// generation to be above after, so a status that still shows the previous
// snapshot is not measured twice. The status can trail the synchronous suspend
// by a moment, so the read is retried until the deadline.
func waitBallastSnapshotAfter(ctx context.Context, kubeContext, actor string, after int64, timeout time.Duration) (ballastActorSnapshot, error) {
	deadline := time.Now().Add(timeout)
	for {
		snapshot, err := latestBallastSnapshot(ctx, kubeContext, actor)
		if err != nil {
			return ballastActorSnapshot{}, err
		}
		if snapshot.Generation > after {
			return snapshot, nil
		}
		if !time.Now().Before(deadline) {
			return ballastActorSnapshot{}, fmt.Errorf(
				"latest snapshot generation is still %d after %s, want greater than %d",
				snapshot.Generation, timeout, after)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ballastActorSnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// ballastActorSnapshot identifies the newest snapshot in an actor's status.
type ballastActorSnapshot struct {
	Generation int64
	URI        string
}

// latestBallastSnapshot reads the actor and returns the snapshot with the
// highest generation among the entries that carry a durable snapshot URI. The
// provider waits for the durable snapshot before Snapshot returns, but the
// status can briefly show a newer generation whose URI is not projected yet;
// skipping those entries lets waitBallastSnapshotAfter poll for the new
// generation instead of failing on an entry that is still being filled in.
func latestBallastSnapshot(ctx context.Context, kubeContext, actor string) (ballastActorSnapshot, error) {
	out, err := kubectlAte(ctx, kubeContext, "get", "actor", actor, "-a", ateSpace, "-o", "json")
	if err != nil {
		return ballastActorSnapshot{}, err
	}

	var document struct {
		Status struct {
			Snapshots []struct {
				Generation      json.RawMessage `json:"generation"`
				DurableSnapshot *struct {
					Object *struct {
						SnapshotURI string `json:"snapshotUri"`
					} `json:"object"`
				} `json:"durableSnapshot"`
			} `json:"snapshots"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return ballastActorSnapshot{}, fmt.Errorf("decode actor %s: %w", actor, err)
	}
	if len(document.Status.Snapshots) == 0 {
		return ballastActorSnapshot{}, fmt.Errorf("actor %s has no status.snapshots", actor)
	}

	found := false
	var best ballastActorSnapshot
	for i, snapshot := range document.Status.Snapshots {
		generation, err := ballastJSONInt64(snapshot.Generation)
		if err != nil {
			return ballastActorSnapshot{}, fmt.Errorf("actor %s status.snapshots[%d] generation: %w", actor, i, err)
		}
		if snapshot.DurableSnapshot == nil || snapshot.DurableSnapshot.Object == nil ||
			snapshot.DurableSnapshot.Object.SnapshotURI == "" {
			continue
		}
		// The list is not documented as ordered, so compare generations. A tie
		// keeps the later entry, which is the most recently written one.
		if !found || generation >= best.Generation {
			found = true
			best = ballastActorSnapshot{
				Generation: generation,
				URI:        snapshot.DurableSnapshot.Object.SnapshotURI,
			}
		}
	}
	if !found {
		return ballastActorSnapshot{}, fmt.Errorf(
			"actor %s has no snapshot with a durableSnapshot.object.snapshotUri", actor)
	}
	return best, nil
}

// ballastJSONInt64 reads an int64 that protojson may encode as a JSON number
// or, because it is a 64-bit integer, as a decimal string. An absent field
// decodes as zero.
func ballastJSONInt64(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return 0, nil
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %q as an integer: %w", text, err)
		}
		return value, nil
	}
	return 0, fmt.Errorf("value %s is neither a number nor a string", raw)
}

// destroyBallastActor is the cleanup path: it uses a fresh context because the
// test context may already be cancelled, and reports failures with t.Errorf
// instead of stopping the remaining cleanup.
func destroyBallastActor(t *testing.T, provider *substrate.Provider, kubeContext, actor string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := provider.Destroy(ctx, actor); err != nil {
		t.Errorf("acceptance: destroy actor %s: %v", actor, err)
		return
	}
	if err := waitActorGone(ctx, kubeContext, actor, actorGoneTimeout); err != nil {
		t.Errorf("acceptance: wait for actor %s to disappear: %v", actor, err)
	}
}

// ballastOptionalInt renders an optional page number for a failure message.
func ballastOptionalInt(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}

// ballastOptionalInt64 renders an optional byte offset for a failure message.
func ballastOptionalInt64(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

// TestBallastStatsUnit checks the percentile and summary arithmetic without a
// cluster. It runs in the acceptance-dryrun style because it needs no image,
// no actor and no network.
func TestBallastStatsUnit(t *testing.T) {
	t.Run("nearest rank", func(t *testing.T) {
		values := []int64{50, 10, 40, 20, 30}
		stats := ballastDurationStats(values)
		if stats.Count != 5 {
			t.Fatalf("count = %d, want 5", stats.Count)
		}
		if stats.MedianMS != 30 {
			t.Errorf("median = %d, want 30", stats.MedianMS)
		}
		if stats.P95MS != 50 {
			t.Errorf("p95 = %d, want 50", stats.P95MS)
		}
		if stats.MaxMS != 50 {
			t.Errorf("max = %d, want 50", stats.MaxMS)
		}
		// The input order must survive: the caller keeps the samples.
		if values[0] != 50 {
			t.Errorf("ballastDurationStats reordered its input: %v", values)
		}
	})

	t.Run("even median", func(t *testing.T) {
		stats := ballastDurationStats([]int64{10, 20, 30, 40})
		if stats.MedianMS != 25 {
			t.Errorf("median = %d, want 25", stats.MedianMS)
		}
		// ceil(0.95*4) = 4, the last value.
		if stats.P95MS != 40 {
			t.Errorf("p95 = %d, want 40", stats.P95MS)
		}
	})

	t.Run("snapshot sizes skip unmeasured", func(t *testing.T) {
		stats := ballastSnapshotSizeStats([]int64{300, 100, 200}, 2)
		if stats.Count != 3 || stats.Unmeasured != 2 {
			t.Fatalf("count = %d, unmeasured = %d, want 3 and 2", stats.Count, stats.Unmeasured)
		}
		if stats.MedianBytes != 200 {
			t.Errorf("median = %d, want 200", stats.MedianBytes)
		}
		if stats.P95Bytes != 300 || stats.MaxBytes != 300 {
			t.Errorf("p95 = %d, max = %d, want 300 and 300", stats.P95Bytes, stats.MaxBytes)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if stats := ballastDurationStats(nil); stats != (ballastStats{}) {
			t.Errorf("empty stats = %+v, want zero", stats)
		}
	})
}
