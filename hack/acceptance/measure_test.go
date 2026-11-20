//go:build acceptance

// Measurements for the suspend/resume acceptance step.
//
// The step records four kinds of data for every suspension:
//
//   - the timings the control plane reports (snapshot_ms, restore_ms,
//     hello_ms), in the suspension record;
//   - the snapshot's size, measured from the rustfs local PV for the
//     generation each suspension created (see snapshotsize_test.go);
//   - the delay from sending a message to the resumed turn starting, and the
//     guest clock read inside that turn (the clock skew interval, whose method
//     is TenCycles' `date -u +%s.%N` request);
//   - observations about old connections: the warden's reconnect log lines and
//     each post-restore turn's error flag and duration. These are recorded, not
//     judged.
//
// Every scenario writes measurements.json below its own working directory.
// TestSuspendResume also writes a combined file, with the same summary shape,
// at the run root.

package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// suspendStatSummary is the sample count, median, p95 and maximum of one
// metric. P95 uses the nearest-rank method: the value at ceil(0.95 * count),
// one-indexed.
type suspendStatSummary struct {
	Count  int   `json:"count"`
	Median int64 `json:"median"`
	P95    int64 `json:"p95"`
	Max    int64 `json:"max"`
}

// suspendClockSkew is one guest clock reading against the host-side turn
// interval. The guest ran `date -u` at some instant between the turn's
// started_at and ended_at on the host clock, so the skew lies in
// [GuestTime - TurnEndedAt, GuestTime - TurnStartedAt].
type suspendClockSkew struct {
	GuestTime     time.Time `json:"guest_time"`
	TurnStartedAt time.Time `json:"turn_started_at"`
	TurnEndedAt   time.Time `json:"turn_ended_at"`
	MinMS         int64     `json:"min_ms"`
	MaxMS         int64     `json:"max_ms"`
}

// suspendTurnObservation is one turn as the measurement sees it.
type suspendTurnObservation struct {
	Index                int               `json:"index"`
	TurnID               string            `json:"turn_id"`
	IsError              bool              `json:"is_error"`
	StartedAt            time.Time         `json:"started_at"`
	EndedAt              *time.Time        `json:"ended_at"`
	DurationMS           int64             `json:"duration_ms"`
	AfterRestore         bool              `json:"after_restore"`
	MessageToTurnStartMS *int64            `json:"message_to_turn_start_ms,omitempty"`
	ClockSkew            *suspendClockSkew `json:"clock_skew,omitempty"`
	Output               string            `json:"output,omitempty"`
}

// suspendRecordMeasurement is one suspension: the timings the control plane
// reported, the snapshot size attached to it, and the observations around its
// resume.
type suspendRecordMeasurement struct {
	Index               int                  `json:"index"`
	StartedAt           time.Time            `json:"started_at"`
	SnapshotMS          *int64               `json:"snapshot_ms"`
	RestoreMS           *int64               `json:"restore_ms"`
	HelloMS             *int64               `json:"hello_ms"`
	Error               string               `json:"error"`
	DetectedSuspendedAt *time.Time           `json:"detected_suspended_at,omitempty"`
	IdleToSnapshotMS    *int64               `json:"idle_to_snapshot_ms,omitempty"`
	SnapshotSize        *snapshotMeasurement `json:"snapshot_size,omitempty"`
	SnapshotNote        string               `json:"snapshot_note,omitempty"`
	ResumeTurnIndex     int                  `json:"resume_turn_index,omitempty"`
	SentWhileSuspended  bool                 `json:"sent_while_suspended"`
}

// suspendConnectionObservation is one log line or note about an old
// connection. Covers says which connection it describes.
type suspendConnectionObservation struct {
	Covers string     `json:"covers"`
	Line   string     `json:"line"`
	Time   *time.Time `json:"time,omitempty"`
}

// suspendObservationTimeline records the instants that bound one long freeze.
// Snapshot completion and restore return come from the control plane's
// "provider call" log, which it writes right after each provider call returns;
// subtracting duration_ms gives the call's start. The suspension record
// supplies the suspension start, because the state machine enters suspending
// before it calls the provider.
type suspendObservationTimeline struct {
	SuspendStartedAt    time.Time  `json:"suspend_started_at"`
	SnapshotCompletedAt *time.Time `json:"snapshot_completed_at,omitempty"`
	ResumeStartedAt     *time.Time `json:"resume_started_at,omitempty"`
	RestoreReturnedAt   *time.Time `json:"restore_returned_at,omitempty"`
	// FrozenMS is restore return minus suspension start: how long the guest
	// did not run. SnapshotToRestoreMS is the part of that interval after the
	// snapshot had completed.
	FrozenMS            *int64 `json:"frozen_ms,omitempty"`
	SnapshotToRestoreMS *int64 `json:"snapshot_to_restore_ms,omitempty"`
	// HoldMS is how long the test waited after observing suspended before
	// sending the wake message.
	HoldMS        int64     `json:"hold_ms"`
	MessageSentAt time.Time `json:"message_sent_at"`

	// HelloAt is restore return plus hello_ms: the control plane accepted the
	// warden's Hello on the new stream.
	HelloAt            *time.Time `json:"hello_at,omitempty"`
	HelloFromRestoreMS *int64     `json:"hello_from_restore_ms,omitempty"`
	HelloFromSuspendMS *int64     `json:"hello_from_suspend_ms,omitempty"`

	// DisconnectAt is the warden log line that reported the old stream
	// closing. ReconnectAt is the first warden log line after restore return;
	// the warden logs no explicit reconnect line, so it is an upper bound on
	// the reconnection.
	DisconnectAt            *time.Time `json:"disconnect_at,omitempty"`
	DisconnectFromSuspendMS *int64     `json:"disconnect_from_suspend_ms,omitempty"`
	DisconnectFromRestoreMS *int64     `json:"disconnect_from_restore_ms,omitempty"`
	DisconnectLine          string     `json:"disconnect_line,omitempty"`
	ReconnectAt             *time.Time `json:"reconnect_at,omitempty"`
	ReconnectFromSuspendMS  *int64     `json:"reconnect_from_suspend_ms,omitempty"`
	ReconnectFromRestoreMS  *int64     `json:"reconnect_from_restore_ms,omitempty"`
	ReconnectLine           string     `json:"reconnect_line,omitempty"`

	// Source says where the instants came from, so a reader can tell an
	// observed instant from a derived one.
	Source string `json:"source"`
}

// criteriaEvidence holds the raw record values a criterion used, so a verdict
// can be checked from measurements.json without the database.
type criteriaEvidence map[string]any

// suspendScenarioMeasurement is everything one scenario recorded.
type suspendScenarioMeasurement struct {
	Scenario   string `json:"scenario"`
	RunID      string `json:"run_id,omitempty"`
	Actor      string `json:"actor,omitempty"`
	ActorImage string `json:"actor_image,omitempty"`
	// ActorImageCommit is the source commit the actor image was built from.
	// RepoHead is the tested repository's HEAD for this run. RepoCommit is kept
	// for the v0.1.0 shape and carries RepoHead, never the image commit.
	ActorImageCommit    string                         `json:"actor_image_commit,omitempty"`
	RepoHead            string                         `json:"repo_head,omitempty"`
	RepoCommit          string                         `json:"repo_commit,omitempty"`
	Criteria            criteriaEvidence               `json:"criteria_evidence,omitempty"`
	Written             bool                           `json:"-"`
	StartedAt           time.Time                      `json:"started_at"`
	FinishedAt          time.Time                      `json:"finished_at"`
	Notes               []string                       `json:"notes,omitempty"`
	SuspendCount        int                            `json:"suspend_count"`
	Suspensions         []suspendRecordMeasurement     `json:"suspensions"`
	Turns               []suspendTurnObservation       `json:"turns"`
	SnapshotSizes       []snapshotMeasurement          `json:"snapshot_sizes,omitempty"`
	SnapshotSizeSummary *snapshotSizeSummary           `json:"snapshot_size_summary,omitempty"`
	ClockSkews          []suspendClockSkew             `json:"clock_skews,omitempty"`
	Connections         []suspendConnectionObservation `json:"connection_observations,omitempty"`
	Timeline            *suspendObservationTimeline    `json:"timeline,omitempty"`
	Summaries           map[string]suspendStatSummary  `json:"summaries"`
	ActorStateAtSuspend string                         `json:"actor_state_at_suspend,omitempty"`
	WorkerListing       string                         `json:"worker_listing,omitempty"`
	// The pool's allocated resources before the run, while suspended and
	// after the resumed turn. They back the capacity release criterion.
	WorkerAllocatedBaseline    *substrateWorkerResources `json:"worker_allocated_baseline,omitempty"`
	WorkerAllocatedAtSuspend   *substrateWorkerResources `json:"worker_allocated_at_suspend,omitempty"`
	WorkerAllocatedAfterResume *substrateWorkerResources `json:"worker_allocated_after_resume,omitempty"`
	ControlPlaneRestarts       int                       `json:"control_plane_restarts,omitempty"`
	ControlPlaneRestartAt      *time.Time                `json:"control_plane_restart_at,omitempty"`
}

// suspendCombinedMeasurements is the file at the run root. Each scenario keeps
// its own record; Summaries pools all scenarios' samples.
type suspendCombinedMeasurements struct {
	GeneratedAt time.Time                     `json:"generated_at"`
	Scenarios   []suspendScenarioMeasurement  `json:"scenarios"`
	Summaries   map[string]suspendStatSummary `json:"summaries"`
}

func newSuspendScenario(name string) *suspendScenarioMeasurement {
	return &suspendScenarioMeasurement{
		Scenario:  name,
		StartedAt: time.Now().UTC(),
		Summaries: map[string]suspendStatSummary{},
	}
}

// setSuspensions copies the suspension records from a run detail, keeping the
// fields this package attached to earlier records (a snapshot size, the
// detection instant and so on) at the same index.
func (m *suspendScenarioMeasurement) setSuspensions(detail runDetail) {
	merged := make([]suspendRecordMeasurement, len(detail.Suspensions))
	for i, s := range detail.Suspensions {
		record := suspendRecordMeasurement{
			Index:      i + 1,
			StartedAt:  s.StartedAt,
			SnapshotMS: s.SnapshotMS,
			RestoreMS:  s.RestoreMS,
			HelloMS:    s.HelloMS,
			Error:      s.Error,
		}
		if i < len(m.Suspensions) {
			previous := m.Suspensions[i]
			record.DetectedSuspendedAt = previous.DetectedSuspendedAt
			record.IdleToSnapshotMS = previous.IdleToSnapshotMS
			record.SnapshotSize = previous.SnapshotSize
			record.SnapshotNote = previous.SnapshotNote
			record.ResumeTurnIndex = previous.ResumeTurnIndex
			record.SentWhileSuspended = previous.SentWhileSuspended
		}
		merged[i] = record
	}
	m.Suspensions = merged
	m.SuspendCount = len(merged)
}

// markSuspensionDetected records when the test first saw the run suspended and
// how long after the previous turn's end the suspension began.
func (m *suspendScenarioMeasurement) markSuspensionDetected(index int, at time.Time, previous *turn) {
	if index < 0 || index >= len(m.Suspensions) {
		return
	}
	detected := at.UTC()
	m.Suspensions[index].DetectedSuspendedAt = &detected
	if previous != nil && previous.EndedAt != nil {
		idle := m.Suspensions[index].StartedAt.Sub(*previous.EndedAt).Milliseconds()
		m.Suspensions[index].IdleToSnapshotMS = &idle
	}
}

// suspendSnapshotTracker remembers the snapshot URIs already accounted for.
// status.snapshots keeps the current and the in-progress generation, so a
// measurement after a suspension may contain the new generation plus the
// previous one; only the URI that was not seen before belongs to this
// suspension.
type suspendSnapshotTracker struct {
	seen map[string]struct{}
}

func newSuspendSnapshotTracker() *suspendSnapshotTracker {
	return &suspendSnapshotTracker{seen: make(map[string]struct{})}
}

// takeNewest returns the last URI not seen in an earlier measurement. Every
// non-empty URI in this observation is then marked as seen, so a later cycle
// cannot claim it again.
func (t *suspendSnapshotTracker) takeNewest(measurements []snapshotMeasurement) (snapshotMeasurement, bool) {
	newest := -1
	for i, measurement := range measurements {
		if measurement.URI == "" {
			continue
		}
		if _, ok := t.seen[measurement.URI]; !ok {
			newest = i
		}
	}
	for _, measurement := range measurements {
		if measurement.URI != "" {
			t.seen[measurement.URI] = struct{}{}
		}
	}
	if newest >= 0 {
		return measurements[newest], true
	}
	// With no URI at all there is no identity to compare. A single entry is
	// the only snapshot the actor reported; record it as unmeasured instead of
	// dropping the observation.
	if len(measurements) == 1 && measurements[0].URI == "" {
		return measurements[0], true
	}
	return snapshotMeasurement{}, false
}

// attachSnapshot records one snapshot measurement for the suspension at index
// and keeps the size summary current. An unmeasured snapshot becomes a note
// instead of a zero-byte measurement.
func (m *suspendScenarioMeasurement) attachSnapshot(index int, measurement snapshotMeasurement) {
	m.SnapshotSizes = append(m.SnapshotSizes, measurement)
	summary := summarizeSnapshotSizes(m.SnapshotSizes)
	m.SnapshotSizeSummary = &summary
	if index < 0 || index >= len(m.Suspensions) {
		return
	}
	if measurement.Measured {
		value := measurement
		m.Suspensions[index].SnapshotSize = &value
		return
	}
	reason := measurement.Error
	if reason == "" {
		reason = "snapshot was not measured"
	}
	m.Suspensions[index].SnapshotNote = reason
}

// addTurn appends one turn observation.
func (m *suspendScenarioMeasurement) addTurn(turn turn, index int, afterRestore bool, sentAt *time.Time, skew *suspendClockSkew) {
	observation := suspendTurnObservation{
		Index:        index,
		TurnID:       turn.TurnID,
		IsError:      turn.IsError,
		StartedAt:    turn.StartedAt,
		EndedAt:      turn.EndedAt,
		AfterRestore: afterRestore,
		ClockSkew:    skew,
		Output:       turn.Output,
	}
	if turn.EndedAt != nil {
		observation.DurationMS = turn.EndedAt.Sub(turn.StartedAt).Milliseconds()
	}
	if sentAt != nil {
		delay := turn.StartedAt.Sub(*sentAt).Milliseconds()
		observation.MessageToTurnStartMS = &delay
	}
	if skew != nil {
		m.ClockSkews = append(m.ClockSkews, *skew)
	}
	m.Turns = append(m.Turns, observation)
}

// addNote appends a note that is visible in measurements.json.
func (m *suspendScenarioMeasurement) addNote(format string, args ...any) {
	m.Notes = append(m.Notes, fmt.Sprintf(format, args...))
}

// addCriteria records one raw value a judge read.
func (m *suspendScenarioMeasurement) addCriteria(key string, value any) {
	if m.Criteria == nil {
		m.Criteria = criteriaEvidence{}
	}
	m.Criteria[key] = value
}

// subscriptionRowEvidence is the subscription fields the judges read.
type subscriptionRowEvidence struct {
	SubID       string     `json:"sub_id"`
	JobID       string     `json:"job_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SinceCursor int64      `json:"since_cursor"`
	CompletedAt *time.Time `json:"completed_at"`
	Status      string     `json:"status"`
	Terminal    string     `json:"terminal"`
}

func subscriptionRowEvidenceOf(subs []subscriptionRecord) []subscriptionRowEvidence {
	out := make([]subscriptionRowEvidence, 0, len(subs))
	for _, sub := range subs {
		out = append(out, subscriptionRowEvidence{
			SubID:       sub.SubID,
			JobID:       criteriaJobID(sub),
			CreatedAt:   sub.CreatedAt,
			SinceCursor: sub.SinceCursor,
			CompletedAt: sub.CompletedAt,
			Status:      sub.Status,
			Terminal:    sub.Terminal,
		})
	}
	return out
}

// connectorEventRowEvidence is the connector_event fields the judges read.
type connectorEventRowEvidence struct {
	AppendSeq   int64             `json:"append_seq"`
	DeliveryID  string            `json:"delivery_id"`
	ReceivedAt  time.Time         `json:"received_at"`
	Correlation map[string]string `json:"correlation,omitempty"`
}

func connectorEventRowEvidenceOf(events []connectorEventRecord) []connectorEventRowEvidence {
	out := make([]connectorEventRowEvidence, 0, len(events))
	for _, event := range events {
		out = append(out, connectorEventRowEvidence{
			AppendSeq:   event.AppendSeq,
			DeliveryID:  event.DeliveryID,
			ReceivedAt:  event.ReceivedAt,
			Correlation: event.Correlation,
		})
	}
	return out
}

// inboxRowEvidence is the inbox fields the judges read, with the completion
// payload's source, terminal and job id pulled out for direct inspection.
type inboxRowEvidence struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source,omitempty"`
	Terminal  string    `json:"terminal,omitempty"`
	JobID     string    `json:"job_id,omitempty"`
}

func inboxRowEvidenceOf(rows []inboxRecord) []inboxRowEvidence {
	out := make([]inboxRowEvidence, 0, len(rows))
	for _, row := range rows {
		evidence := inboxRowEvidence{Seq: row.Seq, Kind: row.Kind, CreatedAt: row.CreatedAt}
		if completion, err := criteriaParseCompletion(row.Payload); err == nil {
			evidence.Source = completion.Source
			evidence.Terminal = completion.Terminal
			evidence.JobID = completion.Correlation["job.id"]
		}
		out = append(out, evidence)
	}
	return out
}

// extsimDeliveryEvidence is one callback attempt as extsim recorded it.
type extsimDeliveryEvidence struct {
	DeliveryID string    `json:"delivery_id"`
	Attempt    int       `json:"attempt"`
	SentAt     time.Time `json:"sent_at"`
	StatusCode int       `json:"status_code"`
	Error      string    `json:"error,omitempty"`
}

func extsimDeliveryEvidenceOf(deliveries []extsimDelivery) []extsimDeliveryEvidence {
	out := make([]extsimDeliveryEvidence, 0, len(deliveries))
	for _, delivery := range deliveries {
		out = append(out, extsimDeliveryEvidence(delivery))
	}
	return out
}

// turnRecordEvidence is one turn as the judges read it.
type turnRecordEvidence struct {
	TurnID     string     `json:"turn_id"`
	DeliveryID string     `json:"delivery_id,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	IsError    bool       `json:"is_error"`
}

func turnRecordEvidenceOf(turns []turn) []turnRecordEvidence {
	out := make([]turnRecordEvidence, 0, len(turns))
	for _, record := range turns {
		out = append(out, turnRecordEvidence{
			TurnID:     record.TurnID,
			DeliveryID: record.DeliveryID,
			StartedAt:  record.StartedAt,
			EndedAt:    record.EndedAt,
			IsError:    record.IsError,
		})
	}
	return out
}

// suspensionRecordEvidence is one suspension as the judges read it.
type suspensionRecordEvidence struct {
	StartedAt         time.Time  `json:"started_at"`
	SnapshotMS        *int64     `json:"snapshot_ms"`
	RestoreMS         *int64     `json:"restore_ms"`
	HelloMS           *int64     `json:"hello_ms"`
	Error             string     `json:"error"`
	RestoreReturnedAt *time.Time `json:"restore_returned_at,omitempty"`
}

func suspensionRecordEvidenceOf(records []suspension) []suspensionRecordEvidence {
	out := make([]suspensionRecordEvidence, 0, len(records))
	for _, record := range records {
		out = append(out, suspensionRecordEvidence{
			StartedAt:         record.StartedAt,
			SnapshotMS:        record.SnapshotMS,
			RestoreMS:         record.RestoreMS,
			HelloMS:           record.HelloMS,
			Error:             record.Error,
			RestoreReturnedAt: record.RestoreReturnedAt,
		})
	}
	return out
}

// recordCriteriaRecords writes the raw record lists every criterion reads.
func recordCriteriaRecords(m *suspendScenarioMeasurement, c criteriaCase, jobID string) {
	m.addCriteria("subscriptions", subscriptionRowEvidenceOf(c.Subscriptions))
	m.addCriteria("inbox", inboxRowEvidenceOf(c.Inbox))
	m.addCriteria("completion_delivery_rows", len(c.CompletionDeliveries))
	if jobID == "" {
		return
	}
	m.addCriteria("connector_events", connectorEventRowEvidenceOf(criteriaEventsForJob(c.ConnectorEvents, jobID)))
	m.addCriteria("extsim_deliveries", extsimDeliveryEvidenceOf(c.ExtsimDeliveries[jobID]))
}

// callbackCompletionSource returns the source of the first subscription
// completion row in the inbox, or "" when there is none.
func callbackCompletionSource(c criteriaCase) string {
	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) == 0 {
		return ""
	}
	completion, err := criteriaParseCompletion(rows[0].Payload)
	if err != nil {
		return ""
	}
	return completion.Source
}

// suspendStatValues gathers every raw sample in a scenario. It is split out so
// the per-scenario summary and the combined summary use the same rules.
type suspendStatValues struct {
	snapshotMS          []int64
	restoreMS           []int64
	helloMS             []int64
	idleToSnapshotMS    []int64
	messageToTurnStart  []int64
	turnDuration        []int64
	restoreTurnDuration []int64
	snapshotBytes       []int64
	clockSkewMin        []int64
	clockSkewMax        []int64
	clockSkewWidth      []int64
	frozenMS            []int64
}

func suspendGatherValues(scenarios []suspendScenarioMeasurement) suspendStatValues {
	var values suspendStatValues
	for _, scenario := range scenarios {
		for _, record := range scenario.Suspensions {
			if record.SnapshotMS != nil {
				values.snapshotMS = append(values.snapshotMS, *record.SnapshotMS)
			}
			if record.RestoreMS != nil {
				values.restoreMS = append(values.restoreMS, *record.RestoreMS)
			}
			if record.HelloMS != nil {
				values.helloMS = append(values.helloMS, *record.HelloMS)
			}
			if record.IdleToSnapshotMS != nil {
				values.idleToSnapshotMS = append(values.idleToSnapshotMS, *record.IdleToSnapshotMS)
			}
			if record.SnapshotSize != nil && record.SnapshotSize.Measured {
				values.snapshotBytes = append(values.snapshotBytes, record.SnapshotSize.Bytes)
			}
		}
		for _, observation := range scenario.Turns {
			if observation.EndedAt != nil {
				values.turnDuration = append(values.turnDuration, observation.DurationMS)
				if observation.AfterRestore {
					values.restoreTurnDuration = append(values.restoreTurnDuration, observation.DurationMS)
				}
			}
			if observation.MessageToTurnStartMS != nil {
				values.messageToTurnStart = append(values.messageToTurnStart, *observation.MessageToTurnStartMS)
			}
		}
		for _, skew := range scenario.ClockSkews {
			values.clockSkewMin = append(values.clockSkewMin, skew.MinMS)
			values.clockSkewMax = append(values.clockSkewMax, skew.MaxMS)
			values.clockSkewWidth = append(values.clockSkewWidth, skew.MaxMS-skew.MinMS)
		}
		if scenario.Timeline != nil && scenario.Timeline.FrozenMS != nil {
			values.frozenMS = append(values.frozenMS, *scenario.Timeline.FrozenMS)
		}
	}
	return values
}

// suspendSummariesFrom computes the summary map. A metric with no samples is
// omitted rather than reported as zero.
func suspendSummariesFrom(values suspendStatValues) map[string]suspendStatSummary {
	summaries := make(map[string]suspendStatSummary)
	add := func(name string, samples []int64) {
		if len(samples) == 0 {
			return
		}
		summaries[name] = suspendSummarizeValues(samples)
	}
	add("snapshot_ms", values.snapshotMS)
	add("restore_ms", values.restoreMS)
	add("hello_ms", values.helloMS)
	add("idle_to_snapshot_ms", values.idleToSnapshotMS)
	add("message_to_turn_start_ms", values.messageToTurnStart)
	add("turn_duration_ms", values.turnDuration)
	add("restore_turn_duration_ms", values.restoreTurnDuration)
	add("snapshot_bytes", values.snapshotBytes)
	add("clock_skew_min_ms", values.clockSkewMin)
	add("clock_skew_max_ms", values.clockSkewMax)
	add("clock_skew_width_ms", values.clockSkewWidth)
	add("frozen_ms", values.frozenMS)
	return summaries
}

// suspendSummarizeValues returns the count, median, p95 and maximum. Median is
// the middle value, or the average of the two middle values for an even count.
func suspendSummarizeValues(values []int64) suspendStatSummary {
	var summary suspendStatSummary
	if len(values) == 0 {
		return summary
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	summary.Count = len(sorted)
	summary.Max = sorted[len(sorted)-1]
	if len(sorted)%2 == 1 {
		summary.Median = sorted[len(sorted)/2]
	} else {
		summary.Median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	rank := (95*len(sorted) + 99) / 100 // ceil(0.95 * n), one-indexed
	summary.P95 = sorted[rank-1]
	return summary
}

func (m *suspendScenarioMeasurement) computeSummaries() {
	m.Summaries = suspendSummariesFrom(suspendGatherValues([]suspendScenarioMeasurement{*m}))
}

// finish stamps the scenario and writes its measurements.json into workDir.
// It is safe to call from a deferred function after a fatal assertion.
func (m *suspendScenarioMeasurement) finish(t *testing.T, workDir string) {
	t.Helper()
	m.FinishedAt = time.Now().UTC()
	m.computeSummaries()
	suspendWriteJSON(t, filepath.Join(workDir, "measurements.json"), m)
	m.Written = true
}

// suspendWriteCombined writes the combined file at the run root. It runs even
// when a scenario failed, so the completed scenarios are still recorded.
func suspendWriteCombined(t *testing.T, scenarios []suspendScenarioMeasurement) {
	t.Helper()
	combined := suspendCombinedMeasurements{
		GeneratedAt: time.Now().UTC(),
		Scenarios:   scenarios,
		Summaries:   suspendSummariesFrom(suspendGatherValues(scenarios)),
	}
	suspendWriteJSON(t, filepath.Join(runRoot(t), "measurements.json"), combined)
}

func suspendWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Errorf("acceptance: encode %s: %v", path, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Errorf("acceptance: create %s: %v", filepath.Dir(path), err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Errorf("acceptance: write %s: %v", path, err)
	}
}
