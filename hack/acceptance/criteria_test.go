//go:build acceptance

// The v0.2.0 pass criteria as pure functions.
//
// Each judge takes the records the acceptance harness collects and returns the
// first place the criterion fails, or "" when it holds. The real local and
// Substrate tests collect the records and call only these functions for their
// verdict; they do not re-check a criterion in the test body. Keeping the
// judges pure makes them testable without a database, a cluster or an LLM.
//
// The input records come from harness_test.go and extsim_test.go: turns,
// suspension records, inbox rows, subscriptions, connector events, completion
// delivery rows, extsim delivery records and bg_jobs changes. A real run also
// observes moments no record carries, such as when the control plane stopped
// and restarted, and generates random values the scenario later reports; those
// travel in criteriaTimes and criteriaExpectations.

package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	criteriaIdleGrace     = 30 * time.Second // G_idle
	criteriaScanPeriod    = 5 * time.Second  // P_scan
	criteriaDeliverPeriod = 5 * time.Second  // P_deliver

	// criteriaDuplicateWindow is the shortest duplicate-delivery observation
	// window: G_idle + P_scan + P_deliver.
	criteriaDuplicateWindow = criteriaIdleGrace + criteriaScanPeriod + criteriaDeliverPeriod

	// criteriaBackgroundHold is how long bg_jobs must stay non-empty at a turn
	// boundary before the background-job criterion can judge the suspension
	// timing: G_idle + P_scan + 10 s. Shorter than this is a failed
	// precondition, not a passing criterion.
	criteriaBackgroundHold = criteriaIdleGrace + criteriaScanPeriod + 10*time.Second

	// criteriaDefaultTurnDeadline bounds how long after the completion event
	// reaches the inbox a turn processing it may start, when the caller does
	// not set a stricter deadline.
	criteriaDefaultTurnDeadline = 5 * time.Minute

	// criteriaReconnectTimeout is T_reconnect, the default control plane wait
	// before asking the provider about an active run whose outbound stream
	// disconnected.
	criteriaReconnectTimeout = 30 * time.Second

	criteriaActorFailurePrefix        = "actor "
	criteriaActorCrashedWord          = "crashed"
	criteriaActorNotFoundWord         = "not found"
	criteriaKindSubscriptionCompleted = "subscription_completed"
	criteriaSourceWebhook             = "webhook"
	criteriaSourceVerify              = "verify"
	criteriaTerminalFailed            = "failed"
	criteriaTerminalSucceeded         = "succeeded"
	criteriaStatusOK                  = 200
	criteriaStateSuspended            = "suspended"
)

// criteriaTimes holds the moments a real run observes that no record carries.
// A judge that needs one of them fails when it is zero.
type criteriaTimes struct {
	// ControlPlaneStoppedAt and ControlPlaneRestartedAt bracket the downtime
	// of the restart criterion.
	ControlPlaneStoppedAt   time.Time
	ControlPlaneRestartedAt time.Time
	// WebhookListeningAt is when the restarted control plane logged its
	// webhook listener.
	WebhookListeningAt time.Time
	// RunStateAtStop is the run's state when the control plane stopped. It is
	// empty when the collector did not observe the state.
	RunStateAtStop string

	// ObservationStartedAt and ObservationEndedAt bracket the duplicate-
	// delivery observation window.
	ObservationStartedAt time.Time
	ObservationEndedAt   time.Time
}

// criteriaExpectations holds the values the scenario generated and the judge
// therefore cannot derive from the records.
type criteriaExpectations struct {
	// JobID is the job a single-job criterion targets.
	JobID string
	// InitialMarker is the random marker the initial content carried.
	InitialMarker string
	// ReplayStatus is the HTTP status of replaying a webhook body with the
	// same delivery identifier.
	ReplayStatus int
	// TurnDeadline overrides criteriaDefaultTurnDeadline when positive.
	TurnDeadline time.Duration
}

// criteriaCase is the collected input of one criterion.
type criteriaCase struct {
	Turns                []turn
	Suspensions          []suspension
	Inbox                []inboxRecord
	Subscriptions        []subscriptionRecord
	ConnectorEvents      []connectorEventRecord
	CompletionDeliveries []completionDeliveryRecord

	// ExtsimDeliveries maps a job ID to every callback attempt extsim made
	// for it. Order does not matter.
	ExtsimDeliveries map[string][]extsimDelivery

	// BGJobs is the bg_jobs observer's change list in observation order.
	BGJobs []bgJobsChange
	// BGJobsResolution is the observer's sampling interval. Zero means
	// defaultBGJobsInterval. The observer timestamps a change when it reads it,
	// so a transition happened somewhere between the previous read and the
	// recorded one; the background-job criterion allows for that resolution.
	BGJobsResolution time.Duration

	// DryRun is set by TestDryRunSubscription. Local mode forces suspension
	// off, so the dry run skips only the suspension checks; every other check
	// of the same criterion still runs on the real records.
	DryRun bool

	Times  criteriaTimes
	Expect criteriaExpectations
}

// criteriaCompletion is the subscription completion event stored in an inbox
// payload or a completion delivery row.
type criteriaCompletion struct {
	SubID              string
	System             string
	Event              string
	SubscriptionStatus string
	Terminal           string
	Correlation        map[string]string
	Source             string
}

// criteriaJobFact is the part of the extsim job object the criteria read from
// a connector event payload.
type criteriaJobFact struct {
	Reason    string
	UpdatedAt time.Time
}

// ---------------------------------------------------------------------------
// The six criteria

// judgeRoundTrip is pass criterion 1: two round trips, each waiting through a
// suspension, with the first submission failing and the second succeeding.
func judgeRoundTrip(c criteriaCase) string {
	if len(c.Subscriptions) != 2 {
		return fmt.Sprintf("run has %d subscriptions, want 2", len(c.Subscriptions))
	}
	wantTerminal := []string{criteriaTerminalFailed, criteriaTerminalSucceeded}
	for i, sub := range c.Subscriptions {
		if sub.Status != "completed" {
			return fmt.Sprintf("subscription %s (%d) has status %q, want completed", sub.SubID, i+1, sub.Status)
		}
		if sub.Terminal != wantTerminal[i] {
			return fmt.Sprintf("subscription %s (%d) has terminal %q, want %q", sub.SubID, i+1, sub.Terminal, wantTerminal[i])
		}
	}

	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) != 2 {
		return fmt.Sprintf("inbox has %d subscription_completed rows, want 2", len(rows))
	}
	completions := make([]criteriaCompletion, len(rows))
	for i, row := range rows {
		completion, err := criteriaParseCompletion(row.Payload)
		if err != nil {
			return fmt.Sprintf("inbox row %d: %v", row.Seq, err)
		}
		completions[i] = completion
		if completion.Terminal != wantTerminal[i] {
			return fmt.Sprintf("inbox row %d has terminal %q, want %q", row.Seq, completion.Terminal, wantTerminal[i])
		}
		if completion.Source != criteriaSourceWebhook {
			return fmt.Sprintf("inbox row %d has source %q, want %q", row.Seq, completion.Source, criteriaSourceWebhook)
		}
	}

	if !c.DryRun {
		if len(c.Suspensions) != 2 {
			return fmt.Sprintf("run has %d suspension records, want 2", len(c.Suspensions))
		}
		for i, record := range c.Suspensions {
			if record.Error != "" {
				return fmt.Sprintf("suspension %d failed: %s", i+1, record.Error)
			}
			if record.SnapshotMS == nil || record.RestoreMS == nil || record.HelloMS == nil {
				return fmt.Sprintf("suspension %d is missing snapshot_ms, restore_ms or hello_ms", i+1)
			}
		}

		for i, record := range c.Suspensions {
			jobID := criteriaJobID(c.Subscriptions[i])
			if jobID == "" {
				return fmt.Sprintf("subscription %d has no job.id in its correlation", i+1)
			}
			callback, ok := criteriaFirstSuccess(c.ExtsimDeliveries[jobID])
			if !ok {
				return fmt.Sprintf("extsim has no successful callback for job %s", jobID)
			}
			snapshotDone := record.StartedAt.Add(time.Duration(*record.SnapshotMS) * time.Millisecond)
			if !callback.SentAt.After(snapshotDone) {
				return fmt.Sprintf("the first 2xx callback for job %s was sent at %s, not after the snapshot completed at %s",
					jobID, criteriaFormatTime(callback.SentAt), criteriaFormatTime(snapshotDone))
			}
			restoreStart, mismatch := criteriaRestoreStart(record)
			if mismatch != "" {
				return fmt.Sprintf("suspension %d: %s", i+1, mismatch)
			}
			if !restoreStart.After(callback.SentAt) {
				return fmt.Sprintf("the restore for suspension %d started at %s, not after the first 2xx callback at %s",
					i+1, criteriaFormatTime(restoreStart), criteriaFormatTime(callback.SentAt))
			}
			previous, ok := criteriaLastTurnEndedAtOrBefore(c.Turns, record.StartedAt)
			if !ok {
				return fmt.Sprintf("no turn ended before suspension %d started at %s", i+1, criteriaFormatTime(record.StartedAt))
			}
			if idle := record.StartedAt.Sub(*previous.EndedAt); idle < criteriaIdleGrace {
				return fmt.Sprintf("suspension %d started at %s, %s after turn %s ended at %s; want at least G_idle %s",
					i+1, criteriaFormatTime(record.StartedAt), idle, previous.TurnID,
					criteriaFormatTime(*previous.EndedAt), criteriaIdleGrace)
			}
		}
	} else {
		// The callback itself is not a suspension-only fact: every round trip
		// still needs a successful callback from extsim.
		for i, sub := range c.Subscriptions {
			jobID := criteriaJobID(sub)
			if jobID == "" {
				return fmt.Sprintf("subscription %d has no job.id in its correlation", i+1)
			}
			if _, ok := criteriaFirstSuccess(c.ExtsimDeliveries[jobID]); !ok {
				return fmt.Sprintf("extsim has no successful callback for job %s", jobID)
			}
		}
	}

	report, ok := criteriaLastTurnStartedAtOrAfter(c.Turns, rows[1].CreatedAt)
	if !ok {
		return fmt.Sprintf("no turn started after the second completion event reached the inbox at %s",
			criteriaFormatTime(rows[1].CreatedAt))
	}
	if report.IsError {
		return fmt.Sprintf("the restored turn %s is an error: %q", report.TurnID, report.Output)
	}

	firstJob := criteriaJobID(c.Subscriptions[0])
	secondJob := criteriaJobID(c.Subscriptions[1])
	fact, ok := criteriaJobFactFor(c.ConnectorEvents, firstJob)
	if !ok {
		return fmt.Sprintf("no connector event records the job object for job %s, so its reason cannot be checked", firstJob)
	}
	if fact.Reason == "" {
		return fmt.Sprintf("the connector event for job %s carries no reason", firstJob)
	}
	for _, want := range []struct{ label, value string }{
		{"the first job id", firstJob},
		{"the first job's failure reason", fact.Reason},
		{"the initial content marker", c.Expect.InitialMarker},
		{"the second job id", secondJob},
		{"the second job's terminal", criteriaTerminalSucceeded},
	} {
		if want.value == "" {
			return fmt.Sprintf("the criterion input is missing %s", want.label)
		}
		if !strings.Contains(report.Output, want.value) {
			return fmt.Sprintf("the restored turn %s does not report %s (%q):\n%s",
				report.TurnID, want.label, want.value, report.Output)
		}
	}
	return ""
}

// judgeCallbackFirst is pass criterion 2: the job completed and its callback
// arrived before the subscription was registered. Registration backtracks,
// matches the event and completes the subscription itself.
func judgeCallbackFirst(c criteriaCase) string {
	if c.Expect.JobID == "" {
		return "the criterion input has no job id"
	}
	sub, ok := criteriaSubscriptionForJob(c.Subscriptions, c.Expect.JobID)
	if !ok {
		return fmt.Sprintf("no subscription for job %s", c.Expect.JobID)
	}
	if sub.Status != "completed" {
		return fmt.Sprintf("subscription %s has status %q at registration, want completed", sub.SubID, sub.Status)
	}
	if sub.CompletedAt == nil {
		return fmt.Sprintf("subscription %s is not completed at registration: completed_at is null", sub.SubID)
	}

	events := criteriaEventsForJob(c.ConnectorEvents, c.Expect.JobID)
	if len(events) == 0 {
		return fmt.Sprintf("no connector_event for job %s", c.Expect.JobID)
	}

	// The registration completes the subscription itself by backtracking to
	// an event that arrived before created_at and sits below since_cursor.
	// Both halves are necessary: completed_at falling close to created_at is
	// not enough, because a later scan could have completed it.
	backtracked := false
	for _, event := range events {
		if event.ReceivedAt.Before(sub.CreatedAt) && event.AppendSeq < sub.SinceCursor {
			backtracked = true
			break
		}
	}
	if !backtracked {
		return fmt.Sprintf("subscription %s did not complete during its registration: no connector_event for job %s both arrived before the subscription was created at %s and has append_seq below since_cursor %d",
			sub.SubID, c.Expect.JobID, criteriaFormatTime(sub.CreatedAt), sub.SinceCursor)
	}

	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) != 1 {
		return fmt.Sprintf("inbox has %d subscription_completed rows, want 1", len(rows))
	}
	completion, err := criteriaParseCompletion(rows[0].Payload)
	if err != nil {
		return fmt.Sprintf("inbox row %d: %v", rows[0].Seq, err)
	}
	if job := completion.Correlation["job.id"]; job != c.Expect.JobID {
		return fmt.Sprintf("completion event correlation job.id is %q, want %q", job, c.Expect.JobID)
	}
	if completion.Source != criteriaSourceWebhook {
		return fmt.Sprintf("completion event source is %q, want %q", completion.Source, criteriaSourceWebhook)
	}
	if _, mismatch := criteriaTurnProcessingEvent(c.Turns, rows[0].CreatedAt, c.Expect.TurnDeadline); mismatch != "" {
		return mismatch
	}
	return ""
}

// judgeCallbackDropped is pass criterion 3: the job finished but its callback
// was dropped. Registration's status query produces the completion event.
func judgeCallbackDropped(c criteriaCase) string {
	if c.Expect.JobID == "" {
		return "the criterion input has no job id"
	}
	deliveries, collected := c.ExtsimDeliveries[c.Expect.JobID]
	if !collected {
		return fmt.Sprintf("extsim deliveries for job %s were not collected", c.Expect.JobID)
	}
	if len(deliveries) != 0 {
		return fmt.Sprintf("extsim has %d callback attempts for job %s, want none", len(deliveries), c.Expect.JobID)
	}
	if events := criteriaEventsForJob(c.ConnectorEvents, c.Expect.JobID); len(events) != 0 {
		return fmt.Sprintf("the connector event log has %d rows for job %s, want none", len(events), c.Expect.JobID)
	}

	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) != 1 {
		return fmt.Sprintf("inbox has %d subscription_completed rows, want 1", len(rows))
	}
	completion, err := criteriaParseCompletion(rows[0].Payload)
	if err != nil {
		return fmt.Sprintf("inbox row %d: %v", rows[0].Seq, err)
	}
	if job := completion.Correlation["job.id"]; job != c.Expect.JobID {
		return fmt.Sprintf("completion event correlation job.id is %q, want %q", job, c.Expect.JobID)
	}
	if completion.Source != criteriaSourceVerify {
		return fmt.Sprintf("completion event source is %q, want %q", completion.Source, criteriaSourceVerify)
	}
	if _, mismatch := criteriaTurnProcessingEvent(c.Turns, rows[0].CreatedAt, c.Expect.TurnDeadline); mismatch != "" {
		return mismatch
	}
	return ""
}

// judgeDuplicateDelivery is pass criterion 4: the same job is delivered twice
// with different delivery identifiers, and the Agent still receives exactly
// one completion event. A same-identifier replay must also answer 200.
func judgeDuplicateDelivery(c criteriaCase) string {
	if c.Expect.JobID == "" {
		return "the criterion input has no job id"
	}

	deliveries, collected := c.ExtsimDeliveries[c.Expect.JobID]
	if !collected {
		return fmt.Sprintf("extsim deliveries for job %s were not collected", c.Expect.JobID)
	}
	successes := 0
	successIDs := make(map[string]bool)
	for _, delivery := range deliveries {
		if !criteriaIsSuccess(delivery.StatusCode) {
			continue
		}
		successes++
		successIDs[delivery.DeliveryID] = true
	}
	if successes != 2 {
		return fmt.Sprintf("extsim has %d 2xx callbacks for job %s, want 2", successes, c.Expect.JobID)
	}
	if len(successIDs) != 2 {
		return fmt.Sprintf("the 2 successful callbacks for job %s used %d delivery ids, want 2 different ids",
			c.Expect.JobID, len(successIDs))
	}

	events := criteriaEventsForJob(c.ConnectorEvents, c.Expect.JobID)
	if len(events) != 2 {
		return fmt.Sprintf("the connector event log has %d rows for job %s, want 2", len(events), c.Expect.JobID)
	}
	if len(c.CompletionDeliveries) != 1 {
		return fmt.Sprintf("run has %d completion_delivery rows, want 1", len(c.CompletionDeliveries))
	}

	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) != 1 {
		return fmt.Sprintf("inbox has %d subscription_completed rows, want 1", len(rows))
	}
	completion, err := criteriaParseCompletion(rows[0].Payload)
	if err != nil {
		return fmt.Sprintf("inbox row %d: %v", rows[0].Seq, err)
	}
	if job := completion.Correlation["job.id"]; job != c.Expect.JobID {
		return fmt.Sprintf("completion event correlation job.id is %q, want %q", job, c.Expect.JobID)
	}

	if c.Expect.ReplayStatus != criteriaStatusOK {
		return fmt.Sprintf("replaying the same delivery id returned %d, want %d",
			c.Expect.ReplayStatus, criteriaStatusOK)
	}

	if c.Times.ObservationStartedAt.IsZero() || c.Times.ObservationEndedAt.IsZero() {
		return "the duplicate-delivery observation window was not recorded"
	}
	if window := c.Times.ObservationEndedAt.Sub(c.Times.ObservationStartedAt); window < criteriaDuplicateWindow {
		return fmt.Sprintf("the observation window lasted %s, want at least G_idle + P_scan + P_deliver = %s",
			window, criteriaDuplicateWindow)
	}
	for _, observed := range c.Turns {
		if observed.StartedAt.After(c.Times.ObservationStartedAt) &&
			!observed.StartedAt.After(c.Times.ObservationEndedAt) {
			return fmt.Sprintf("turn %s started at %s, inside the observation window starting at %s; the turn count must not grow",
				observed.TurnID, criteriaFormatTime(observed.StartedAt),
				criteriaFormatTime(c.Times.ObservationStartedAt))
		}
	}

	if mismatch := criteriaTurnInboxMismatch(c.Turns, c.Inbox); mismatch != "" {
		return mismatch
	}
	return ""
}

// judgeRestartWhileSuspended is pass criterion 5: the control plane restarts
// while the run is suspended; extsim retries the callback after the restart and
// the run resumes to process it.
func judgeRestartWhileSuspended(c criteriaCase) string {
	if c.Expect.JobID == "" {
		return "the criterion input has no job id"
	}
	if c.Times.ControlPlaneStoppedAt.IsZero() || c.Times.ControlPlaneRestartedAt.IsZero() ||
		c.Times.WebhookListeningAt.IsZero() {
		return "the criterion input is missing the control plane stop, restart or webhook listening moment"
	}
	if !c.DryRun {
		if c.Times.RunStateAtStop == "" {
			return fmt.Sprintf("the run state at the control plane stop at %s was not recorded",
				criteriaFormatTime(c.Times.ControlPlaneStoppedAt))
		}
		if c.Times.RunStateAtStop != criteriaStateSuspended {
			return fmt.Sprintf("the run was %q when the control plane stopped at %s, want %q",
				c.Times.RunStateAtStop, criteriaFormatTime(c.Times.ControlPlaneStoppedAt), criteriaStateSuspended)
		}
	}

	sub, ok := criteriaSubscriptionForJob(c.Subscriptions, c.Expect.JobID)
	if !ok {
		return fmt.Sprintf("no subscription for job %s", c.Expect.JobID)
	}
	if sub.Status != "completed" {
		return fmt.Sprintf("subscription %s has status %q, want completed", sub.SubID, sub.Status)
	}

	events := criteriaEventsForJob(c.ConnectorEvents, c.Expect.JobID)
	if len(events) == 0 {
		return fmt.Sprintf("no connector_event for job %s", c.Expect.JobID)
	}
	fact, ok := criteriaJobFactFor(c.ConnectorEvents, c.Expect.JobID)
	if !ok || fact.UpdatedAt.IsZero() {
		return fmt.Sprintf("the connector event for job %s does not record the job's updated_at", c.Expect.JobID)
	}
	if !fact.UpdatedAt.After(c.Times.ControlPlaneStoppedAt) || !fact.UpdatedAt.Before(c.Times.ControlPlaneRestartedAt) {
		return fmt.Sprintf("job %s was updated at %s, not between the control plane stop at %s and restart at %s",
			c.Expect.JobID, criteriaFormatTime(fact.UpdatedAt),
			criteriaFormatTime(c.Times.ControlPlaneStoppedAt), criteriaFormatTime(c.Times.ControlPlaneRestartedAt))
	}

	deliveries, collected := c.ExtsimDeliveries[c.Expect.JobID]
	if !collected {
		return fmt.Sprintf("extsim deliveries for job %s were not collected", c.Expect.JobID)
	}
	if mismatch := criteriaRetryAfterRestart(deliveries,
		c.Times.ControlPlaneRestartedAt, c.Times.WebhookListeningAt); mismatch != "" {
		return mismatch
	}

	rows := criteriaCompletionInbox(c.Inbox)
	if len(rows) != 1 {
		return fmt.Sprintf("inbox has %d subscription_completed rows, want 1", len(rows))
	}
	completion, err := criteriaParseCompletion(rows[0].Payload)
	if err != nil {
		return fmt.Sprintf("inbox row %d: %v", rows[0].Seq, err)
	}
	if job := completion.Correlation["job.id"]; job != c.Expect.JobID {
		return fmt.Sprintf("completion event correlation job.id is %q, want %q", job, c.Expect.JobID)
	}
	if completion.Source != criteriaSourceWebhook {
		return fmt.Sprintf("completion event source is %q, want %q", completion.Source, criteriaSourceWebhook)
	}
	if !rows[0].CreatedAt.After(c.Times.ControlPlaneRestartedAt) {
		return fmt.Sprintf("the completion event reached the inbox at %s, not after the restart at %s",
			criteriaFormatTime(rows[0].CreatedAt), criteriaFormatTime(c.Times.ControlPlaneRestartedAt))
	}

	if !c.DryRun {
		suspendedBefore := false
		for _, record := range c.Suspensions {
			if record.StartedAt.Before(c.Times.ControlPlaneStoppedAt) {
				suspendedBefore = true
				break
			}
		}
		if !suspendedBefore {
			return fmt.Sprintf("no suspension started before the control plane stopped at %s",
				criteriaFormatTime(c.Times.ControlPlaneStoppedAt))
		}
	}
	if _, mismatch := criteriaTurnProcessingEvent(c.Turns, rows[0].CreatedAt, c.Expect.TurnDeadline); mismatch != "" {
		return mismatch
	}
	return ""
}

// judgeBackgroundJob is pass criterion 6: a background job keeps the run from
// suspending; after it ends and a spontaneous turn appears, the run suspends
// only after the idle grace. The run has no subscription.
func judgeBackgroundJob(c criteriaCase) string {
	if len(c.Subscriptions) != 0 {
		return fmt.Sprintf("run has %d subscriptions, want none in the background-job criterion", len(c.Subscriptions))
	}

	resolution := c.BGJobsResolution
	if resolution <= 0 {
		resolution = defaultBGJobsInterval
	}

	if !c.DryRun {
		for i, record := range c.Suspensions {
			jobs, ok := criteriaBGJobsAt(c.BGJobs, record.StartedAt)
			if !ok {
				return fmt.Sprintf("no bg_jobs observation covers suspension %d's start at %s",
					i+1, criteriaFormatTime(record.StartedAt))
			}
			if len(jobs) != 0 {
				return fmt.Sprintf("suspension %d started at %s while bg_jobs was non-empty: %v",
					i+1, criteriaFormatTime(record.StartedAt), jobs)
			}
		}
	}

	held, ok := criteriaBackgroundHoldAtBoundary(criteriaNonEmptyIntervals(c.BGJobs), c.Turns, resolution)
	if !ok {
		return "precondition not met: no turn boundary held until bg_jobs became empty without another turn starting in between, so the no-suspend rule was not observed"
	}
	if held < criteriaBackgroundHold {
		return fmt.Sprintf("precondition not met: bg_jobs stayed non-empty at a turn boundary for %s, want at least G_idle + P_scan + 10 s = %s",
			held, criteriaBackgroundHold)
	}

	emptiedAt, ok := criteriaFirstEmptyAfterNonEmpty(c.BGJobs)
	if !ok {
		return "bg_jobs never became empty after being non-empty"
	}
	// The observer timestamps a change when it reads it, so the transition
	// happened after the previous read, which is at most one sampling
	// interval before the recorded empty time. A turn the driver started right
	// after the empty push can therefore carry a start time just before the
	// recorded empty time; the previous read is the strictest moment the
	// observation supports.
	emptiedNoEarlierThan := emptiedAt.Add(-resolution)
	spontaneous, ok := criteriaFirstTurnStartedAfter(c.Turns, emptiedNoEarlierThan)
	if !ok {
		return fmt.Sprintf("no spontaneous turn started after bg_jobs became empty (observed at %s, not before %s)",
			criteriaFormatTime(emptiedAt), criteriaFormatTime(emptiedNoEarlierThan))
	}
	// A turn the control plane starts from a background task notification is
	// not answering an inbox row, so it carries no delivery_id. A turn with
	// one is an ordinary delivered turn and cannot be the spontaneous turn.
	if spontaneous.DeliveryID != "" {
		return fmt.Sprintf("the spontaneous turn %s has delivery_id %q; a spontaneous turn starts from a background task notification, not from an inbox delivery",
			spontaneous.TurnID, spontaneous.DeliveryID)
	}
	if spontaneous.EndedAt == nil {
		return fmt.Sprintf("the spontaneous turn %s started after bg_jobs became empty at %s, but it has not ended",
			spontaneous.TurnID, criteriaFormatTime(emptiedAt))
	}
	if !c.DryRun {
		suspension, ok := criteriaFirstSuspensionStartedAfter(c.Suspensions, emptiedAt)
		if !ok {
			return fmt.Sprintf("no suspension started after bg_jobs became empty at %s", criteriaFormatTime(emptiedAt))
		}
		if !suspension.StartedAt.After(emptiedAt) {
			return fmt.Sprintf("suspension started at %s, not after bg_jobs became empty at %s",
				criteriaFormatTime(suspension.StartedAt), criteriaFormatTime(emptiedAt))
		}
		if suspension.Error != "" {
			return fmt.Sprintf("suspension 1 failed: %s", suspension.Error)
		}
		grace := spontaneous.EndedAt.Add(criteriaIdleGrace)
		if suspension.StartedAt.Before(grace) {
			return fmt.Sprintf("suspension started at %s, %s after the spontaneous turn %s ended at %s; want at least G_idle %s",
				criteriaFormatTime(suspension.StartedAt), suspension.StartedAt.Sub(*spontaneous.EndedAt),
				spontaneous.TurnID, criteriaFormatTime(*spontaneous.EndedAt), criteriaIdleGrace)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Record helpers

// criteriaCompletionInbox returns the subscription_completed rows, keeping the
// inbox_seq order.
func criteriaCompletionInbox(inbox []inboxRecord) []inboxRecord {
	var rows []inboxRecord
	for _, row := range inbox {
		if row.Kind == criteriaKindSubscriptionCompleted {
			rows = append(rows, row)
		}
	}
	return rows
}

// criteriaParseCompletion decodes a subscription completion payload. The
// documents fix the fields but not their JSON spelling, so both the snake_case
// form and Go's default field names are accepted.
func criteriaParseCompletion(data []byte) (criteriaCompletion, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return criteriaCompletion{}, fmt.Errorf("the subscription completion payload is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return criteriaCompletion{}, fmt.Errorf("decode the subscription completion payload: %w", err)
	}
	text := func(keys ...string) string {
		for _, key := range keys {
			raw, ok := fields[key]
			if !ok {
				continue
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				continue
			}
			return value
		}
		return ""
	}
	stringMap := func(keys ...string) map[string]string {
		for _, key := range keys {
			raw, ok := fields[key]
			if !ok {
				continue
			}
			var value map[string]string
			if err := json.Unmarshal(raw, &value); err != nil {
				continue
			}
			return value
		}
		return nil
	}
	return criteriaCompletion{
		SubID:              text("sub_id", "SubID"),
		System:             text("system", "System"),
		Event:              text("event", "Event"),
		SubscriptionStatus: text("subscription_status", "SubscriptionStatus"),
		Terminal:           text("terminal", "Terminal"),
		Correlation:        stringMap("correlation", "Correlation"),
		Source:             text("source", "Source"),
	}, nil
}

// criteriaJobID returns the job identifier a subscription is correlated with.
func criteriaJobID(sub subscriptionRecord) string {
	return sub.Correlation["job.id"]
}

// criteriaSubscriptionForJob returns the subscription whose job.id is jobID.
func criteriaSubscriptionForJob(subscriptions []subscriptionRecord, jobID string) (subscriptionRecord, bool) {
	for _, sub := range subscriptions {
		if criteriaJobID(sub) == jobID {
			return sub, true
		}
	}
	return subscriptionRecord{}, false
}

// criteriaEventsForJob returns the connector events correlated with jobID in
// append order.
func criteriaEventsForJob(events []connectorEventRecord, jobID string) []connectorEventRecord {
	var matched []connectorEventRecord
	for _, event := range events {
		if event.Correlation["job.id"] == jobID {
			matched = append(matched, event)
		}
	}
	return matched
}

// criteriaJobFactFor reads the extsim job object from the first connector
// event correlated with jobID.
func criteriaJobFactFor(events []connectorEventRecord, jobID string) (criteriaJobFact, bool) {
	for _, event := range events {
		if event.Correlation["job.id"] != jobID {
			continue
		}
		var body struct {
			Job struct {
				Reason    string `json:"reason"`
				UpdatedAt string `json:"updated_at"`
			} `json:"job"`
		}
		if err := json.Unmarshal(event.Payload, &body); err != nil {
			continue
		}
		fact := criteriaJobFact{Reason: body.Job.Reason}
		if body.Job.UpdatedAt != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, body.Job.UpdatedAt); err == nil {
				fact.UpdatedAt = parsed
			}
		}
		return fact, true
	}
	return criteriaJobFact{}, false
}

// criteriaIsSuccess reports whether status is a 2xx HTTP status.
func criteriaIsSuccess(status int) bool {
	return status >= 200 && status < 300
}

// criteriaFirstSuccess returns the earliest delivery attempt that got a 2xx.
func criteriaFirstSuccess(deliveries []extsimDelivery) (extsimDelivery, bool) {
	var first extsimDelivery
	found := false
	for _, delivery := range deliveries {
		if !criteriaIsSuccess(delivery.StatusCode) {
			continue
		}
		if !found || delivery.SentAt.Before(first.SentAt) {
			first = delivery
			found = true
		}
	}
	return first, found
}

// criteriaRestoreStart returns when the restore of a suspension began. The
// suspension table records restore_returned_at and restore_ms; subtracting the
// duration gives the moment the restore started. A missing field is a failed
// observation, not a start at the completion event: falling back to the inbox
// row would make the "the restore started after the callback" check true by
// construction, because that row is what started the restore.
func criteriaRestoreStart(record suspension) (time.Time, string) {
	if record.RestoreReturnedAt == nil {
		return time.Time{}, "the suspension record has no restore_returned_at"
	}
	if record.RestoreMS == nil {
		return time.Time{}, "the suspension record has no restore_ms"
	}
	return record.RestoreReturnedAt.Add(-time.Duration(*record.RestoreMS) * time.Millisecond), ""
}

// criteriaRetryAfterRestart checks that one delivery identifier failed before
// the control plane restarted and later got a 2xx after the restarted webhook
// listener came up.
func criteriaRetryAfterRestart(deliveries []extsimDelivery, restartedAt, listeningAt time.Time) string {
	failedBefore := make(map[string]bool)
	for _, delivery := range deliveries {
		if !criteriaIsSuccess(delivery.StatusCode) && delivery.SentAt.Before(restartedAt) {
			failedBefore[delivery.DeliveryID] = true
		}
	}
	if len(failedBefore) == 0 {
		return fmt.Sprintf("extsim has no failed callback attempt before the control plane restarted at %s",
			criteriaFormatTime(restartedAt))
	}
	for _, delivery := range deliveries {
		if criteriaIsSuccess(delivery.StatusCode) && delivery.SentAt.After(listeningAt) && failedBefore[delivery.DeliveryID] {
			return ""
		}
	}
	return fmt.Sprintf("no delivery id both failed before the restart and got a 2xx after the restarted webhook listener came up at %s",
		criteriaFormatTime(listeningAt))
}

// criteriaLastTurnEndedAtOrBefore returns the turn that ended last at or before
// at, or false when no turn had ended by then.
func criteriaLastTurnEndedAtOrBefore(turns []turn, at time.Time) (*turn, bool) {
	var latest *turn
	for i := range turns {
		candidate := &turns[i]
		if candidate.EndedAt == nil || candidate.EndedAt.After(at) {
			continue
		}
		if latest == nil || candidate.EndedAt.After(*latest.EndedAt) {
			latest = candidate
		}
	}
	return latest, latest != nil
}

// criteriaLastTurnStartedAtOrAfter returns the turn that started last at or
// after at, or false when none started by then.
func criteriaLastTurnStartedAtOrAfter(turns []turn, at time.Time) (*turn, bool) {
	var latest *turn
	for i := range turns {
		candidate := &turns[i]
		if candidate.StartedAt.Before(at) {
			continue
		}
		if latest == nil || candidate.StartedAt.After(latest.StartedAt) {
			latest = candidate
		}
	}
	return latest, latest != nil
}

// criteriaFirstTurnStartedAfter returns the earliest turn that started strictly
// after at.
func criteriaFirstTurnStartedAfter(turns []turn, at time.Time) (*turn, bool) {
	var first *turn
	for i := range turns {
		candidate := &turns[i]
		if !candidate.StartedAt.After(at) {
			continue
		}
		if first == nil || candidate.StartedAt.Before(first.StartedAt) {
			first = candidate
		}
	}
	return first, first != nil
}

// criteriaFirstSuspensionStartedAfter returns the earliest suspension that
// started strictly after at.
func criteriaFirstSuspensionStartedAfter(suspensions []suspension, at time.Time) (*suspension, bool) {
	var first *suspension
	for i := range suspensions {
		candidate := &suspensions[i]
		if !candidate.StartedAt.After(at) {
			continue
		}
		if first == nil || candidate.StartedAt.Before(first.StartedAt) {
			first = candidate
		}
	}
	return first, first != nil
}

// criteriaTurnProcessingEvent returns the first non-error turn that started
// between the event reaching the inbox and the deadline after it.
func criteriaTurnProcessingEvent(turns []turn, inboxAt time.Time, deadline time.Duration) (*turn, string) {
	if deadline <= 0 {
		deadline = criteriaDefaultTurnDeadline
	}
	limit := inboxAt.Add(deadline)
	var errorTurn *turn
	for i := range turns {
		candidate := &turns[i]
		if candidate.StartedAt.Before(inboxAt) || candidate.StartedAt.After(limit) {
			continue
		}
		if candidate.IsError {
			errorTurn = candidate
			continue
		}
		return candidate, ""
	}
	if errorTurn != nil {
		return nil, fmt.Sprintf("the only turn inside the deadline after the event (turn %s at %s) is an error: %q",
			errorTurn.TurnID, criteriaFormatTime(errorTurn.StartedAt), errorTurn.Output)
	}
	return nil, fmt.Sprintf("no turn started between the event reaching the inbox at %s and the deadline at %s",
		criteriaFormatTime(inboxAt), criteriaFormatTime(limit))
}

// criteriaTurnInboxMismatch checks the one-to-one correspondence the duplicate-
// delivery criterion inherits from the v0.1.0 turn/inbox check: as many turns
// as inbox rows, each turn started by a distinct delivery, and each turn
// starting no earlier than the inbox row it answers. Both slices are in order.
func criteriaTurnInboxMismatch(turns []turn, inbox []inboxRecord) string {
	if len(turns) != len(inbox) {
		return fmt.Sprintf("run has %d turns for %d inbox rows, want one turn per message", len(turns), len(inbox))
	}
	seen := make(map[string]int, len(turns))
	for i, record := range turns {
		if record.DeliveryID == "" {
			return fmt.Sprintf("turn %s has no delivery_id; every turn here starts from an inbox message", record.TurnID)
		}
		if previous, ok := seen[record.DeliveryID]; ok {
			return fmt.Sprintf("delivery_id %q started both turn %d and turn %d; each message must be delivered once",
				record.DeliveryID, previous+1, i+1)
		}
		seen[record.DeliveryID] = i
		if record.StartedAt.Before(inbox[i].CreatedAt) {
			return fmt.Sprintf("turn %s started at %s, before inbox row %d was created at %s",
				record.TurnID, criteriaFormatTime(record.StartedAt), inbox[i].Seq,
				criteriaFormatTime(inbox[i].CreatedAt))
		}
	}
	return ""
}

// criteriaBGJobsAt returns the bg_jobs set in effect at at, and false when no
// observation covers that moment.
func criteriaBGJobsAt(changes []bgJobsChange, at time.Time) ([]string, bool) {
	var jobs []string
	found := false
	for _, change := range changes {
		if change.At.After(at) {
			break
		}
		jobs = change.Jobs
		found = true
	}
	return jobs, found
}

// criteriaBGJobsInterval is one contiguous stretch in which bg_jobs was
// non-empty.
type criteriaBGJobsInterval struct {
	Start time.Time
	End   time.Time
	Jobs  []string
}

// criteriaNonEmptyIntervals returns each contiguous non-empty stretch. A set
// still non-empty at the last observation ends at that observation.
func criteriaNonEmptyIntervals(changes []bgJobsChange) []criteriaBGJobsInterval {
	var intervals []criteriaBGJobsInterval
	var start time.Time
	var jobs []string
	active := false
	for _, change := range changes {
		nonEmpty := len(change.Jobs) > 0
		switch {
		case nonEmpty && !active:
			active = true
			start = change.At
			jobs = change.Jobs
		case !nonEmpty && active:
			intervals = append(intervals, criteriaBGJobsInterval{Start: start, End: change.At, Jobs: jobs})
			active = false
		}
	}
	if active && len(changes) > 0 {
		intervals = append(intervals, criteriaBGJobsInterval{
			Start: start,
			End:   changes[len(changes)-1].At,
			Jobs:  jobs,
		})
	}
	return intervals
}

// criteriaBackgroundHoldAtBoundary returns the longest guaranteed idle span
// that starts at a turn boundary inside a non-empty stretch and lasts until
// the stretch emptied. A stretch that another turn interrupts contributes
// nothing: the criterion asks whether the run stayed idle past G_idle with
// bg_jobs non-empty, and an intervening turn restarts the idle period, so the
// rule was never exercised over that span.
//
// resolution is the observer's sampling interval: the recorded change time is
// when the observer read it, so the set may have emptied up to one interval
// before the recorded empty time. One interval before that time is the latest
// moment the observation guarantees the set was still non-empty. A turn that
// started after that moment may be the spontaneous turn that ran after the
// set emptied, read slightly early, so it is neither a boundary nor an
// interruption. Among the remaining turns, the last one to end before that
// moment is the boundary: an earlier turn is followed by a later turn, which
// restarts the idle period, so no single idle span runs from it to the empty
// transition. The stretch's recorded start is uncertain in the same way, so a
// turn that ended up to one interval before it can still have ended with the
// set non-empty.
func criteriaBackgroundHoldAtBoundary(intervals []criteriaBGJobsInterval, turns []turn, resolution time.Duration) (time.Duration, bool) {
	if resolution < 0 {
		resolution = 0
	}
	var longest time.Duration
	found := false
	for _, interval := range intervals {
		windowStart := interval.Start.Add(-resolution)
		windowEnd := interval.End.Add(-resolution)

		var boundary *turn
		for i := range turns {
			ended := turns[i].EndedAt
			if ended == nil || ended.Before(windowStart) || ended.After(windowEnd) {
				continue
			}
			if turns[i].StartedAt.After(windowEnd) {
				continue
			}
			if boundary == nil || ended.After(*boundary.EndedAt) {
				boundary = &turns[i]
			}
		}
		if boundary == nil {
			continue
		}

		interrupted := false
		for i := range turns {
			if &turns[i] == boundary {
				continue
			}
			started := turns[i].StartedAt
			if started.After(*boundary.EndedAt) && !started.After(windowEnd) {
				interrupted = true
				break
			}
		}
		if interrupted {
			continue
		}

		held := windowEnd.Sub(*boundary.EndedAt)
		if !found || held > longest {
			longest = held
			found = true
		}
	}
	return longest, found
}

// criteriaFirstEmptyAfterNonEmpty returns when bg_jobs first became empty after
// having been non-empty.
func criteriaFirstEmptyAfterNonEmpty(changes []bgJobsChange) (time.Time, bool) {
	previousNonEmpty := false
	for _, change := range changes {
		nonEmpty := len(change.Jobs) > 0
		if previousNonEmpty && !nonEmpty {
			return change.At, true
		}
		previousNonEmpty = nonEmpty
	}
	return time.Time{}, false
}

// criteriaFormatTime formats a moment for a mismatch message.
func criteriaFormatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
