//go:build acceptance

// Record-level tests for the six v0.2.0 pass criteria. Every criterion has at
// least one set of records it accepts and at least one counterexample aimed at
// its decisive check. The records are built in memory; no database, cluster or
// LLM is involved, so these run in acceptance-dryrun.

package acceptance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Builders

func criteriaTestBase() time.Time {
	return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
}

func criteriaTestJSON(t testing.TB, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test record: %v", err)
	}
	return data
}

func criteriaTestCompletionPayload(t testing.TB, subID, jobID, terminal, source string) []byte {
	t.Helper()
	return criteriaTestJSON(t, map[string]any{
		"sub_id":              subID,
		"system":              "extsim",
		"event":               "job.completed",
		"subscription_status": "completed",
		"terminal":            terminal,
		"correlation":         map[string]string{"job.id": jobID, "job.project": "demo"},
		"source":              source,
	})
}

func criteriaTestConnectorPayload(t testing.TB, jobID, conclusion, reason string, updatedAt time.Time) []byte {
	t.Helper()
	job := map[string]any{
		"id":         jobID,
		"project":    "demo",
		"status":     "completed",
		"conclusion": conclusion,
	}
	if reason != "" {
		job["reason"] = reason
	}
	if !updatedAt.IsZero() {
		job["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	}
	return criteriaTestJSON(t, map[string]any{"job": job})
}

func criteriaTestTurn(id, delivery string, startedAt, endedAt time.Time, output string) turn {
	return turn{
		TurnID:     id,
		DeliveryID: delivery,
		StartedAt:  startedAt,
		EndedAt:    &endedAt,
		Output:     output,
	}
}

func criteriaTestSuspension(startedAt time.Time, snapshotMS, restoreMS, helloMS int64) suspension {
	return suspension{
		StartedAt:  startedAt,
		SnapshotMS: &snapshotMS,
		RestoreMS:  &restoreMS,
		HelloMS:    &helloMS,
	}
}

func criteriaTestInboxRow(seq int64, kind, text string, createdAt time.Time, payload []byte) inboxRecord {
	return inboxRecord{Seq: seq, Kind: kind, Text: text, Payload: payload, CreatedAt: createdAt}
}

func criteriaTestSubscription(subID, jobID, terminal string, createdAt time.Time, sinceCursor int64) subscriptionRecord {
	completedAt := createdAt.Add(time.Second)
	return subscriptionRecord{
		SubID:       subID,
		System:      "extsim",
		Event:       "job.completed",
		Status:      "completed",
		Terminal:    terminal,
		Correlation: map[string]string{"job.id": jobID, "job.project": "demo"},
		SinceCursor: sinceCursor,
		CreatedAt:   createdAt,
		CompletedAt: &completedAt,
	}
}

func criteriaTestConnectorEvent(seq int64, jobID string, receivedAt time.Time, payload []byte) connectorEventRecord {
	return connectorEventRecord{
		AppendSeq:   seq,
		System:      "extsim",
		DeliveryID:  fmt.Sprintf("webhook-%d", seq),
		EventType:   "job.completed",
		ProjectIDs:  []string{"default"},
		Correlation: map[string]string{"job.id": jobID, "job.project": "demo"},
		OccurredAt:  receivedAt,
		ReceivedAt:  receivedAt,
		Payload:     payload,
	}
}

func criteriaTestDelivery(id string, attempt int, sentAt time.Time, status int) extsimDelivery {
	return extsimDelivery{DeliveryID: id, Attempt: attempt, SentAt: sentAt, StatusCode: status}
}

func criteriaTestCompletionDelivery(t testing.TB, runID, subID string, createdAt time.Time, payload []byte) completionDeliveryRecord {
	t.Helper()
	deliveredAt := createdAt.Add(time.Second)
	return completionDeliveryRecord{
		SubID:       subID,
		RunID:       runID,
		Status:      "delivered",
		Attempts:    1,
		Event:       payload,
		CreatedAt:   createdAt,
		DeliveredAt: &deliveredAt,
	}
}

func criteriaTestBGChange(at time.Time, jobs ...string) bgJobsChange {
	return bgJobsChange{At: at, Jobs: jobs}
}

// ---------------------------------------------------------------------------
// Criterion 1: two round trips

func criteriaRoundTripCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }

	job1, job2 := "job-aaaa", "job-bbbb"
	marker := "marker-7f3a"
	reason := `missing required field "owner"`

	// The suspension table records when the restore returned; subtract
	// restore_ms to get the restore start, which here equals the inbox row's
	// creation time.
	suspension1 := criteriaTestSuspension(at(40*time.Second), 5000, 5000, 1000)
	restoreReturned1 := at(65 * time.Second)
	suspension1.RestoreReturnedAt = &restoreReturned1
	suspension2 := criteriaTestSuspension(at(100*time.Second), 5000, 5000, 1000)
	restoreReturned2 := at(125 * time.Second)
	suspension2.RestoreReturnedAt = &restoreReturned2

	return criteriaCase{
		Subscriptions: []subscriptionRecord{
			criteriaTestSubscription("sub-1", job1, criteriaTerminalFailed, at(20*time.Second), 5),
			criteriaTestSubscription("sub-2", job2, criteriaTerminalSucceeded, at(80*time.Second), 8),
		},
		Inbox: []inboxRecord{
			criteriaTestInboxRow(1, "human_message", "prompt", at(0), nil),
			criteriaTestInboxRow(2, criteriaKindSubscriptionCompleted, "", at(60*time.Second),
				criteriaTestCompletionPayload(t, "sub-1", job1, criteriaTerminalFailed, criteriaSourceWebhook)),
			criteriaTestInboxRow(3, criteriaKindSubscriptionCompleted, "", at(120*time.Second),
				criteriaTestCompletionPayload(t, "sub-2", job2, criteriaTerminalSucceeded, criteriaSourceWebhook)),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-1", at(0), at(10*time.Second), "submitted the first job"),
			criteriaTestTurn("turn-2", "delivery-2", at(60*time.Second), at(70*time.Second), "corrected and resubmitted"),
			criteriaTestTurn("turn-3", "delivery-3", at(121*time.Second), at(130*time.Second),
				fmt.Sprintf("first job %s failed: %s; marker %s; second job %s succeeded", job1, reason, marker, job2)),
		},
		Suspensions: []suspension{
			suspension1,
			suspension2,
		},
		ConnectorEvents: []connectorEventRecord{
			criteriaTestConnectorEvent(5, job1, at(50*time.Second),
				criteriaTestConnectorPayload(t, job1, criteriaTerminalFailed, reason, time.Time{})),
			criteriaTestConnectorEvent(6, job2, at(110*time.Second),
				criteriaTestConnectorPayload(t, job2, criteriaTerminalSucceeded, "", time.Time{})),
		},
		ExtsimDeliveries: map[string][]extsimDelivery{
			job1: {criteriaTestDelivery("webhook-1", 1, at(50*time.Second), 200)},
			job2: {criteriaTestDelivery("webhook-2", 1, at(110*time.Second), 200)},
		},
		Expect: criteriaExpectations{InitialMarker: marker},
	}
}

func TestJudgeTwoRoundTrips(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeRoundTrip(criteriaRoundTripCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "restore start is not recorded",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].RestoreReturnedAt = nil
			},
			want: "no restore_returned_at",
		},
		{
			name: "callback before the snapshot completes",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-aaaa"] = []extsimDelivery{
					criteriaTestDelivery("webhook-1", 1, criteriaTestBase().Add(44*time.Second), 200),
				}
			},
			want: "not after the snapshot completed",
		},
		{
			name: "restore started before the callback succeeded",
			mutate: func(_ *testing.T, c *criteriaCase) {
				returned := criteriaTestBase().Add(54 * time.Second)
				c.Suspensions[0].RestoreReturnedAt = &returned
			},
			want: "not after the first 2xx callback",
		},
		{
			name: "wrong terminal order",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Subscriptions[0].Terminal = criteriaTerminalSucceeded
			},
			want: "want \"failed\"",
		},
		{
			name: "completion source is verify",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-1", "job-aaaa",
					criteriaTerminalFailed, criteriaSourceVerify)
			},
			want: "want \"webhook\"",
		},
		{
			name: "incomplete suspension record",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].HelloMS = nil
			},
			want: "missing snapshot_ms, restore_ms or hello_ms",
		},
		{
			name: "idle grace too short",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0] = criteriaTestSuspension(
					criteriaTestBase().Add(39*time.Second), 5000, 5000, 1000)
				returned := criteriaTestBase().Add(64 * time.Second)
				c.Suspensions[0].RestoreReturnedAt = &returned
			},
			want: "want at least G_idle",
		},
		{
			name: "restored turn omits the failure reason",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns[2].Output = "first job job-aaaa failed; marker marker-7f3a; second job job-bbbb succeeded"
			},
			want: "does not report the first job's failure reason",
		},
		{
			name: "one subscription",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Subscriptions = c.Subscriptions[:1]
			},
			want: "want 2",
		},
		{
			name: "three subscriptions",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Subscriptions = append(c.Subscriptions,
					criteriaTestSubscription("sub-3", "job-cccc", criteriaTerminalSucceeded,
						criteriaTestBase().Add(140*time.Second), 12))
			},
			want: "want 2",
		},
		{
			name: "one completion row",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Inbox = c.Inbox[:2]
			},
			want: "want 2",
		},
		{
			name: "three completion rows",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox = append(c.Inbox, criteriaTestInboxRow(4, criteriaKindSubscriptionCompleted, "",
					criteriaTestBase().Add(140*time.Second),
					criteriaTestCompletionPayload(t, "sub-3", "job-cccc",
						criteriaTerminalSucceeded, criteriaSourceWebhook)))
			},
			want: "want 2",
		},
		{
			name: "one suspension record",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions = c.Suspensions[:1]
			},
			want: "want 2",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaRoundTripCase(t)
			tc.mutate(t, &c)
			mismatch := judgeRoundTrip(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 2: the callback arrived first

func criteriaCallbackFirstCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }
	job := "job-first"

	return criteriaCase{
		Subscriptions: []subscriptionRecord{
			criteriaTestSubscription("sub-first", job, criteriaTerminalSucceeded, at(50*time.Second), 10),
		},
		ConnectorEvents: []connectorEventRecord{
			criteriaTestConnectorEvent(5, job, at(40*time.Second),
				criteriaTestConnectorPayload(t, job, criteriaTerminalSucceeded, "", time.Time{})),
		},
		Inbox: []inboxRecord{
			criteriaTestInboxRow(1, "human_message", "prompt", at(0), nil),
			criteriaTestInboxRow(2, criteriaKindSubscriptionCompleted, "", at(60*time.Second),
				criteriaTestCompletionPayload(t, "sub-first", job, criteriaTerminalSucceeded, criteriaSourceWebhook)),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-1", at(55*time.Second), at(58*time.Second), "subscribed"),
			criteriaTestTurn("turn-2", "delivery-2", at(61*time.Second), at(70*time.Second), "reported the result"),
		},
		Expect: criteriaExpectations{JobID: job, TurnDeadline: time.Minute},
	}
}

func TestJudgeCallbackFirst(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeCallbackFirst(criteriaCallbackFirstCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	t.Run("satisfied by the backtrack hit alone", func(t *testing.T) {
		c := criteriaCallbackFirstCase(t)
		late := c.Subscriptions[0].CreatedAt.Add(time.Minute)
		c.Subscriptions[0].CompletedAt = &late
		if mismatch := judgeCallbackFirst(c); mismatch != "" {
			t.Fatalf("a backtrack hit outside the registration window was rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "source is verify",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-first", "job-first",
					criteriaTerminalSucceeded, criteriaSourceVerify)
			},
			want: "want \"webhook\"",
		},
		{
			name: "event received after the subscription was created and the completion is late",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ConnectorEvents[0].ReceivedAt = criteriaTestBase().Add(55 * time.Second)
				late := c.Subscriptions[0].CreatedAt.Add(time.Minute)
				c.Subscriptions[0].CompletedAt = &late
			},
			want: "did not complete during its registration",
		},
		{
			// The backtrack is the necessary condition. completed_at falls
			// inside the old registration window, but the event is at
			// since_cursor, so the registration's backtrack never matched it.
			name: "completed within the registration window without a backtrack hit",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ConnectorEvents[0].AppendSeq = 10
			},
			want: "did not complete during its registration",
		},
		{
			name: "append_seq not below since_cursor and the completion is late",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ConnectorEvents[0].AppendSeq = 10
				late := c.Subscriptions[0].CreatedAt.Add(time.Minute)
				c.Subscriptions[0].CompletedAt = &late
			},
			want: "did not complete during its registration",
		},
		{
			name: "subscription was not completed at registration",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Subscriptions[0].Status = "active"
				c.Subscriptions[0].CompletedAt = nil
			},
			want: "want completed",
		},
		{
			name: "no completion rows",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Inbox = c.Inbox[:1]
			},
			want: "want 1",
		},
		{
			name: "two completion rows",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox = append(c.Inbox, criteriaTestInboxRow(3, criteriaKindSubscriptionCompleted, "",
					criteriaTestBase().Add(80*time.Second),
					criteriaTestCompletionPayload(t, "sub-first", "job-first",
						criteriaTerminalSucceeded, criteriaSourceWebhook)))
			},
			want: "want 1",
		},
		{
			name: "completion event is correlated with another job",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-first", "job-other",
					criteriaTerminalSucceeded, criteriaSourceWebhook)
			},
			want: "correlation job.id",
		},
		{
			name: "no turn within the deadline",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns = c.Turns[:1]
			},
			want: "no turn started",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaCallbackFirstCase(t)
			tc.mutate(t, &c)
			mismatch := judgeCallbackFirst(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 3: the callback was dropped

func criteriaCallbackDroppedCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }
	job := "job-dropped"

	return criteriaCase{
		Subscriptions: []subscriptionRecord{
			criteriaTestSubscription("sub-dropped", job, criteriaTerminalSucceeded, at(50*time.Second), 10),
		},
		Inbox: []inboxRecord{
			criteriaTestInboxRow(1, "human_message", "prompt", at(0), nil),
			criteriaTestInboxRow(2, criteriaKindSubscriptionCompleted, "", at(60*time.Second),
				criteriaTestCompletionPayload(t, "sub-dropped", job, criteriaTerminalSucceeded, criteriaSourceVerify)),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-1", at(55*time.Second), at(58*time.Second), "subscribed"),
			criteriaTestTurn("turn-2", "delivery-2", at(61*time.Second), at(70*time.Second), "reported the result"),
		},
		ExtsimDeliveries: map[string][]extsimDelivery{job: {}},
		Expect:           criteriaExpectations{JobID: job, TurnDeadline: time.Minute},
	}
}

func TestJudgeCallbackDropped(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeCallbackDropped(criteriaCallbackDroppedCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "extsim delivered a callback",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-dropped"] = []extsimDelivery{
					criteriaTestDelivery("webhook-1", 1, criteriaTestBase().Add(50*time.Second), 200),
				}
			},
			want: "want none",
		},
		{
			name: "deliveries were not collected",
			mutate: func(_ *testing.T, c *criteriaCase) {
				delete(c.ExtsimDeliveries, "job-dropped")
			},
			want: "were not collected",
		},
		{
			name: "connector event log has the job",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.ConnectorEvents = []connectorEventRecord{
					criteriaTestConnectorEvent(5, "job-dropped", criteriaTestBase().Add(50*time.Second),
						criteriaTestConnectorPayload(t, "job-dropped", criteriaTerminalSucceeded, "", time.Time{})),
				}
			},
			want: "want none",
		},
		{
			name: "source is webhook",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-dropped", "job-dropped",
					criteriaTerminalSucceeded, criteriaSourceWebhook)
			},
			want: "want \"verify\"",
		},
		{
			name: "no completion rows",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Inbox = c.Inbox[:1]
			},
			want: "want 1",
		},
		{
			name: "completion event is correlated with another job",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-dropped", "job-other",
					criteriaTerminalSucceeded, criteriaSourceVerify)
			},
			want: "correlation job.id",
		},
		{
			name: "two completion rows",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox = append(c.Inbox, criteriaTestInboxRow(3, criteriaKindSubscriptionCompleted, "",
					criteriaTestBase().Add(80*time.Second),
					criteriaTestCompletionPayload(t, "sub-dropped", "job-dropped",
						criteriaTerminalSucceeded, criteriaSourceVerify)))
			},
			want: "want 1",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaCallbackDroppedCase(t)
			tc.mutate(t, &c)
			mismatch := judgeCallbackDropped(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 4: duplicate delivery

func criteriaDuplicateDeliveryCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }
	job := "job-dup"

	return criteriaCase{
		Subscriptions: []subscriptionRecord{
			criteriaTestSubscription("sub-dup", job, criteriaTerminalSucceeded, at(5*time.Second), 10),
		},
		ConnectorEvents: []connectorEventRecord{
			criteriaTestConnectorEvent(3, job, at(10*time.Second),
				criteriaTestConnectorPayload(t, job, criteriaTerminalSucceeded, "", time.Time{})),
			criteriaTestConnectorEvent(4, job, at(20*time.Second),
				criteriaTestConnectorPayload(t, job, criteriaTerminalSucceeded, "", time.Time{})),
		},
		Inbox: []inboxRecord{
			criteriaTestInboxRow(1, "human_message", "prompt", at(0), nil),
			criteriaTestInboxRow(2, criteriaKindSubscriptionCompleted, "", at(30*time.Second),
				criteriaTestCompletionPayload(t, "sub-dup", job, criteriaTerminalSucceeded, criteriaSourceWebhook)),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-alpha", at(0), at(5*time.Second), "subscribed"),
			criteriaTestTurn("turn-2", "delivery-beta", at(31*time.Second), at(40*time.Second), "reported the result"),
		},
		CompletionDeliveries: []completionDeliveryRecord{
			criteriaTestCompletionDelivery(t, "run-dup", "sub-dup", at(30*time.Second),
				criteriaTestCompletionPayload(t, "sub-dup", job, criteriaTerminalSucceeded, criteriaSourceWebhook)),
		},
		ExtsimDeliveries: map[string][]extsimDelivery{
			job: {
				criteriaTestDelivery("webhook-a", 1, at(10*time.Second), 200),
				criteriaTestDelivery("webhook-b", 1, at(20*time.Second), 200),
			},
		},
		Times: criteriaTimes{
			ObservationStartedAt: at(45 * time.Second),
			ObservationEndedAt:   at(85 * time.Second),
		},
		Expect: criteriaExpectations{JobID: job, ReplayStatus: criteriaStatusOK},
	}
}

func TestJudgeDuplicateDelivery(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeDuplicateDelivery(criteriaDuplicateDeliveryCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "an extra completion row reached the inbox",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox = append(c.Inbox, criteriaTestInboxRow(3, criteriaKindSubscriptionCompleted, "",
					criteriaTestBase().Add(50*time.Second),
					criteriaTestCompletionPayload(t, "sub-dup", "job-dup",
						criteriaTerminalSucceeded, criteriaSourceWebhook)))
			},
			want: "want 1",
		},
		{
			name: "no completion rows",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Inbox = c.Inbox[:1]
			},
			want: "want 1",
		},
		{
			name: "completion event is correlated with another job",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-dup", "job-other",
					criteriaTerminalSucceeded, criteriaSourceWebhook)
			},
			want: "correlation job.id",
		},
		{
			name: "deliveries were not collected",
			mutate: func(_ *testing.T, c *criteriaCase) {
				delete(c.ExtsimDeliveries, "job-dup")
			},
			want: "were not collected",
		},
		{
			name: "one 2xx callback",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-dup"] = c.ExtsimDeliveries["job-dup"][:1]
			},
			want: "want 2",
		},
		{
			name: "three 2xx callbacks over two delivery ids",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-dup"] = append(c.ExtsimDeliveries["job-dup"],
					criteriaTestDelivery("webhook-a", 2, criteriaTestBase().Add(25*time.Second), 200))
			},
			want: "want 2",
		},
		{
			name: "both 2xx callbacks used the same delivery id",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-dup"][1].DeliveryID = "webhook-a"
			},
			want: "want 2 different ids",
		},
		{
			name: "one connector event row",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ConnectorEvents = c.ConnectorEvents[:1]
			},
			want: "want 2",
		},
		{
			name: "two completion delivery rows",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.CompletionDeliveries = append(c.CompletionDeliveries,
					criteriaTestCompletionDelivery(t, "run-dup", "sub-dup",
						criteriaTestBase().Add(31*time.Second),
						criteriaTestCompletionPayload(t, "sub-dup", "job-dup",
							criteriaTerminalSucceeded, criteriaSourceWebhook)))
			},
			want: "want 1",
		},
		{
			name: "no completion delivery rows",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.CompletionDeliveries = nil
			},
			want: "want 1",
		},
		{
			name: "same-delivery replay did not answer 200",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Expect.ReplayStatus = 500
			},
			want: "want 200",
		},
		{
			name: "observation window too short",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Times.ObservationEndedAt = c.Times.ObservationStartedAt.Add(39 * time.Second)
			},
			want: "want at least",
		},
		{
			name: "a turn started inside the observation window",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns[1].StartedAt = criteriaTestBase().Add(50 * time.Second)
			},
			want: "inside the observation window",
		},
		{
			name: "two turns share one delivery id",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns[1].DeliveryID = c.Turns[0].DeliveryID
			},
			want: "each message must be delivered once",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaDuplicateDeliveryCase(t)
			tc.mutate(t, &c)
			mismatch := judgeDuplicateDelivery(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 5: control plane restart

func criteriaRestartCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }
	job := "job-restart"
	updatedAt := at(55 * time.Second)

	return criteriaCase{
		Subscriptions: []subscriptionRecord{
			criteriaTestSubscription("sub-restart", job, criteriaTerminalSucceeded, at(5*time.Second), 10),
		},
		ConnectorEvents: []connectorEventRecord{
			criteriaTestConnectorEvent(7, job, at(65*time.Second),
				criteriaTestConnectorPayload(t, job, criteriaTerminalSucceeded, "", updatedAt)),
		},
		Inbox: []inboxRecord{
			criteriaTestInboxRow(1, "human_message", "prompt", at(0), nil),
			criteriaTestInboxRow(2, criteriaKindSubscriptionCompleted, "", at(71*time.Second),
				criteriaTestCompletionPayload(t, "sub-restart", job, criteriaTerminalSucceeded, criteriaSourceWebhook)),
		},
		Suspensions: []suspension{
			criteriaTestSuspension(at(10*time.Second), 5000, 5000, 1000),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-1", at(0), at(5*time.Second), "subscribed"),
			criteriaTestTurn("turn-2", "delivery-2", at(72*time.Second), at(80*time.Second), "reported the result"),
		},
		ExtsimDeliveries: map[string][]extsimDelivery{
			job: {
				criteriaTestDelivery("webhook-r", 1, at(55*time.Second), 0),
				criteriaTestDelivery("webhook-r", 2, at(70*time.Second), 200),
			},
		},
		Times: criteriaTimes{
			ControlPlaneStoppedAt:   at(50 * time.Second),
			ControlPlaneRestartedAt: at(60 * time.Second),
			WebhookListeningAt:      at(61 * time.Second),
			RunStateAtStop:          criteriaStateSuspended,
		},
		Expect: criteriaExpectations{JobID: job, TurnDeadline: time.Minute},
	}
}

func TestJudgeControlPlaneRestart(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeRestartWhileSuspended(criteriaRestartCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "job updated outside the downtime",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.ConnectorEvents[0].Payload = criteriaTestConnectorPayload(t, "job-restart",
					criteriaTerminalSucceeded, "", criteriaTestBase().Add(45*time.Second))
			},
			want: "not between the control plane stop",
		},
		{
			name: "the state at the stop was not recorded",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Times.RunStateAtStop = ""
			},
			want: "was not recorded",
		},
		{
			name: "the run was not suspended when the control plane stopped",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Times.RunStateAtStop = "active"
			},
			want: "want \"suspended\"",
		},
		{
			name: "deliveries were not collected",
			mutate: func(_ *testing.T, c *criteriaCase) {
				delete(c.ExtsimDeliveries, "job-restart")
			},
			want: "were not collected",
		},
		{
			name: "no failed attempt before the restart",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-restart"] = []extsimDelivery{
					criteriaTestDelivery("webhook-r", 1, criteriaTestBase().Add(70*time.Second), 200),
				}
			},
			want: "no failed callback attempt",
		},
		{
			name: "the 2xx came before the restarted listener",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.ExtsimDeliveries["job-restart"][1].SentAt = criteriaTestBase().Add(59 * time.Second)
			},
			want: "after the restarted webhook listener",
		},
		{
			name: "source is verify",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-restart", "job-restart",
					criteriaTerminalSucceeded, criteriaSourceVerify)
			},
			want: "want \"webhook\"",
		},
		{
			name: "the run was not suspended before the stop",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].StartedAt = criteriaTestBase().Add(65 * time.Second)
			},
			want: "no suspension started before the control plane stopped",
		},
		{
			name: "no completion rows",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Inbox = c.Inbox[:1]
			},
			want: "want 1",
		},
		{
			name: "two completion rows",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox = append(c.Inbox, criteriaTestInboxRow(3, criteriaKindSubscriptionCompleted, "",
					criteriaTestBase().Add(80*time.Second),
					criteriaTestCompletionPayload(t, "sub-restart", "job-restart",
						criteriaTerminalSucceeded, criteriaSourceWebhook)))
			},
			want: "want 1",
		},
		{
			name: "completion event is correlated with another job",
			mutate: func(t *testing.T, c *criteriaCase) {
				c.Inbox[1].Payload = criteriaTestCompletionPayload(t, "sub-restart", "job-other",
					criteriaTerminalSucceeded, criteriaSourceWebhook)
			},
			want: "correlation job.id",
		},
		{
			name: "no turn after the restart",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns = c.Turns[:1]
			},
			want: "no turn started",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaRestartCase(t)
			tc.mutate(t, &c)
			mismatch := judgeRestartWhileSuspended(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 6: background job

func criteriaBackgroundJobCase(t testing.TB) criteriaCase {
	t.Helper()
	base := criteriaTestBase()
	at := func(d time.Duration) time.Time { return base.Add(d) }

	return criteriaCase{
		BGJobs: []bgJobsChange{
			criteriaTestBGChange(at(0)),
			criteriaTestBGChange(at(5*time.Second), "job-bg"),
			criteriaTestBGChange(at(60 * time.Second)),
		},
		Turns: []turn{
			criteriaTestTurn("turn-1", "delivery-1", at(0), at(10*time.Second), "started the background watch"),
			// The spontaneous turn starts from the background task's
			// notification, so it carries no inbox delivery identifier.
			criteriaTestTurn("turn-2", "", at(61*time.Second), at(70*time.Second), "reported the result"),
		},
		Suspensions: []suspension{
			criteriaTestSuspension(at(100*time.Second), 5000, 5000, 1000),
		},
	}
}

func TestJudgeBackgroundJob(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		if mismatch := judgeBackgroundJob(criteriaBackgroundJobCase(t)); mismatch != "" {
			t.Fatalf("satisfied records were rejected: %s", mismatch)
		}
	})

	counterexamples := []struct {
		name   string
		mutate func(*testing.T, *criteriaCase)
		want   string
	}{
		{
			name: "a suspension began while bg_jobs was non-empty",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions = append(c.Suspensions,
					criteriaTestSuspension(criteriaTestBase().Add(30*time.Second), 5000, 5000, 1000))
			},
			want: "while bg_jobs was non-empty",
		},
		{
			name: "the background hold precondition did not hold",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.BGJobs[2].At = criteriaTestBase().Add(20 * time.Second)
			},
			want: "precondition not met",
		},
		{
			name: "no spontaneous turn after the set emptied",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns = c.Turns[:1]
			},
			want: "no spontaneous turn",
		},
		{
			name: "the suspension ignored the idle grace after the spontaneous turn",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].StartedAt = criteriaTestBase().Add(95 * time.Second)
			},
			want: "want at least G_idle",
		},
		{
			// A suspension that starts while the spontaneous turn is still
			// running is not after that turn at all.
			name: "the suspension started during the spontaneous turn",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].StartedAt = criteriaTestBase().Add(65 * time.Second)
			},
			want: "the spontaneous turn",
		},
		{
			name: "the suspension record carries an error",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].Error = "snapshot failed"
			},
			want: "suspension 1 failed",
		},
		{
			name: "the spontaneous turn has a delivery id",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Turns[1].DeliveryID = "delivery-2"
			},
			want: "spontaneous turn",
		},
		{
			name: "the suspension started when bg_jobs became empty, not after",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Suspensions[0].StartedAt = criteriaTestBase().Add(60 * time.Second)
			},
			want: "no suspension started after bg_jobs became empty",
		},
		{
			name: "the run has a subscription",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.Subscriptions = []subscriptionRecord{
					criteriaTestSubscription("sub-unexpected", "job-unexpected",
						criteriaTerminalSucceeded, criteriaTestBase(), 1),
				}
			},
			want: "want none",
		},
		{
			name: "no bg_jobs observation covers the suspension",
			mutate: func(_ *testing.T, c *criteriaCase) {
				c.BGJobs = nil
			},
			want: "no bg_jobs observation covers",
		},
	}

	for _, tc := range counterexamples {
		t.Run(tc.name, func(t *testing.T) {
			c := criteriaBackgroundJobCase(t)
			tc.mutate(t, &c)
			mismatch := judgeBackgroundJob(c)
			if mismatch == "" {
				t.Fatal("counterexample was accepted")
			}
			if !strings.Contains(mismatch, tc.want) {
				t.Fatalf("mismatch %q does not contain %q", mismatch, tc.want)
			}
		})
	}
}
