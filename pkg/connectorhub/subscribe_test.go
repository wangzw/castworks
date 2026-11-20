package connectorhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

func validSubscribeRequest() subscribeRequest {
	return subscribeRequest{
		System:      "extsim",
		Event:       "job.completed",
		Correlation: map[string]string{"job.id": "j-1", "job.project": "demo"},
	}
}

func readSubscribeResponse(t *testing.T, body []byte) subscribeResponse {
	t.Helper()
	var response subscribeResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode subscribe response %q: %v", body, err)
	}
	if response.SubID == "" || response.Status == "" {
		t.Fatalf("subscribe response = %+v, want sub_id and status", response)
	}
	return response
}

func countSubscriptions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscription`).Scan(&count); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	return count
}

func activeTestEndpoint(t *testing.T, connector Connector) (*connectorHandler, *pgxpool.Pool) {
	t.Helper()
	return newTestEndpoint(t, connector,
		runsWithStates(map[string]runcontroller.State{"run-1": runcontroller.StateActive}))
}

func TestEndpointSubscribeValidation(t *testing.T) {
	t.Parallel()

	handler, pool := activeTestEndpoint(t, &fakeConnector{})
	ctx := t.Context()

	cases := []struct {
		name   string
		body   any
		raw    string
		phrase string
	}{
		{
			name:   "missing system",
			body:   subscribeRequest{Event: "job.completed"},
			phrase: "system is required",
		},
		{
			name: "unknown system",
			body: subscribeRequest{
				System: "github", Event: "job.completed",
				Correlation: map[string]string{"job.id": "j-1"},
			},
			phrase: "not available",
		},
		{
			name:   "missing event",
			body:   subscribeRequest{System: "extsim"},
			phrase: "event is required",
		},
		{
			name: "event outside the catalogue",
			body: subscribeRequest{
				System: "extsim", Event: "job.failed",
				Correlation: map[string]string{"job.id": "j-1"},
			},
			phrase: "not in the event catalogue",
		},
		{
			name: "correlation key not declared",
			body: subscribeRequest{
				System: "extsim", Event: "job.completed",
				Correlation: map[string]string{"job.id": "j-1", "repo": "acme/repo"},
			},
			phrase: "not accepted",
		},
		{
			name: "empty correlation key",
			body: subscribeRequest{
				System: "extsim", Event: "job.completed",
				Correlation: map[string]string{"": "j-1", "job.id": "j-1"},
			},
			phrase: "must not be empty",
		},
		{
			name: "correlation above the size limit",
			body: subscribeRequest{
				System: "extsim", Event: "job.completed",
				Correlation: map[string]string{
					"job.id":      strings.Repeat("x", runcontroller.MaxCorrelationBytes),
					"job.project": "demo",
				},
			},
			phrase: "larger than",
		},
		{
			name: "no identity key",
			body: subscribeRequest{
				System: "extsim", Event: "job.completed",
				Correlation: map[string]string{"job.project": "demo"},
			},
			phrase: "identity key",
		},
		{
			name: "project binding value not bound",
			body: subscribeRequest{
				System: "extsim", Event: "job.completed",
				Correlation: map[string]string{"job.id": "j-1", "job.project": "other"},
			},
			phrase: "not bound",
		},
		{
			name:   "body is not json",
			raw:    "not json",
			phrase: "not valid JSON",
		},
		{
			name:   "trailing json",
			raw:    `{"system":"extsim"}{}`,
			phrase: "trailing",
		},
	}

	for _, tc := range cases {
		var rec *httptest.ResponseRecorder
		if tc.raw != "" {
			rec = doEndpointRawRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", strings.NewReader(tc.raw))
		} else {
			rec = doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", tc.body)
		}

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
			t.Errorf("%s Content-Type = %q, want application/json", tc.name, contentType)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s decode error body %q: %v", tc.name, rec.Body.String(), err)
			continue
		}
		if !strings.Contains(body.Error, tc.phrase) {
			t.Errorf("%s error = %q, want it to contain %q", tc.name, body.Error, tc.phrase)
		}
	}

	if got := countSubscriptions(t, ctx, pool); got != 0 {
		t.Fatalf("subscriptions = %d after rejected requests, want 0", got)
	}
}

func TestSubscribeBackfillBeforeSnapshot(t *testing.T) {
	t.Parallel()

	handler, pool := activeTestEndpoint(t, &fakeConnector{})
	ctx := t.Context()

	occurredAt := testNow().Add(-time.Minute)
	event := defaultTestEvent(0, "d-1", occurredAt)
	appendTestConnectorEvent(t, ctx, pool, event)

	rec := doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", validSubscribeRequest())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	response := readSubscribeResponse(t, rec.Body.Bytes())
	if response.Status != "completed" {
		t.Fatalf("status = %q, want completed", response.Status)
	}
	if response.SinceCursor != 2 {
		t.Fatalf("since_cursor = %d, want 2", response.SinceCursor)
	}

	status, terminal, _ := readTestSubscription(t, ctx, pool, response.SubID)
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed and succeeded", status, terminal)
	}
	if got := countTestRows(t, ctx, pool, "subscription_claim", response.SubID); got != 1 {
		t.Errorf("claims = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", response.SubID); got != 1 {
		t.Fatalf("completion deliveries = %d, want 1", got)
	}
	completion := loadTestCompletions(t, ctx, pool, response.SubID)[0]
	if completion.Source != "webhook" || completion.Terminal != "succeeded" {
		t.Errorf("completion = source %q terminal %q, want webhook and succeeded", completion.Source, completion.Terminal)
	}
	if !completion.OccurredAt.Equal(occurredAt) {
		t.Errorf("completion occurred_at = %v, want %v", completion.OccurredAt, occurredAt)
	}
	if completion.EventID != `["`+response.SubID+`","succeeded"]` {
		t.Errorf("completion event_id = %q", completion.EventID)
	}
}

func TestSubscribeBetweenSnapshotAndRescan(t *testing.T) {
	t.Parallel()

	handler, pool := activeTestEndpoint(t, &fakeConnector{})
	ctx := t.Context()

	occurredAt := testNow().Add(-time.Minute)
	handler.afterSnapshot = func() {
		appendTestConnectorEvent(t, ctx, pool, defaultTestEvent(0, "d-1", occurredAt))
	}

	rec := doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", validSubscribeRequest())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	response := readSubscribeResponse(t, rec.Body.Bytes())
	if response.Status != "completed" {
		t.Fatalf("status = %q, want completed", response.Status)
	}
	if response.SinceCursor != 1 {
		t.Fatalf("since_cursor = %d, want 1", response.SinceCursor)
	}

	status, terminal, _ := readTestSubscription(t, ctx, pool, response.SubID)
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed and succeeded", status, terminal)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", response.SubID); got != 1 {
		t.Fatalf("completion deliveries = %d, want 1", got)
	}
	completion := loadTestCompletions(t, ctx, pool, response.SubID)[0]
	if completion.Source != "webhook" {
		t.Errorf("completion source = %q, want webhook", completion.Source)
	}
	if !completion.OccurredAt.Equal(occurredAt) {
		t.Errorf("completion occurred_at = %v, want %v", completion.OccurredAt, occurredAt)
	}
}

func TestSubscribeAfterRescan(t *testing.T) {
	t.Parallel()

	handler, pool := activeTestEndpoint(t, &fakeConnector{})
	ctx := t.Context()

	rec := doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", validSubscribeRequest())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	response := readSubscribeResponse(t, rec.Body.Bytes())
	if response.Status != "active" {
		t.Fatalf("status = %q, want active before the event arrives", response.Status)
	}
	if response.SinceCursor != 1 {
		t.Fatalf("since_cursor = %d, want 1", response.SinceCursor)
	}

	event := appendTestConnectorEvent(t, ctx, pool, defaultTestEvent(0, "d-1", testNow().Add(-time.Minute)))
	match := func(ev connectorEvent) int {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin match transaction: %v", err)
		}
		completed, err := matchEvent(ctx, tx, ev, jobConclusionOrFail(t, ev.Payload), testNow())
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("matchEvent: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit match: %v", err)
		}
		return completed
	}

	if completed := match(event); completed != 1 {
		t.Fatalf("first match completed %d subscriptions, want 1", completed)
	}
	status, terminal, _ := readTestSubscription(t, ctx, pool, response.SubID)
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed and succeeded", status, terminal)
	}

	duplicate := event
	duplicate.DeliveryID = "d-2"
	appendTestConnectorEvent(t, ctx, pool, duplicate)
	if completed := match(duplicate); completed != 0 {
		t.Fatalf("duplicate match completed %d subscriptions, want 0", completed)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", response.SubID); got != 1 {
		t.Fatalf("completion deliveries = %d after the duplicate, want 1", got)
	}
}

func TestSubscribeStatusQueryCompletesDroppedCallback(t *testing.T) {
	t.Parallel()

	statusBody := []byte(`{"id":"j-1","status":"completed","conclusion":"succeeded"}`)
	connector := &fakeConnector{terminal: "succeeded", reached: true, body: statusBody}
	handler, pool := activeTestEndpoint(t, connector)
	ctx := t.Context()

	rec := doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", validSubscribeRequest())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	response := readSubscribeResponse(t, rec.Body.Bytes())
	if response.Status != "completed" {
		t.Fatalf("status = %q, want completed from the status query", response.Status)
	}

	status, terminal, _ := readTestSubscription(t, ctx, pool, response.SubID)
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed and succeeded", status, terminal)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", response.SubID); got != 1 {
		t.Fatalf("completion deliveries = %d, want 1", got)
	}

	completion := loadTestCompletions(t, ctx, pool, response.SubID)[0]
	if completion.Source != "verify" {
		t.Errorf("completion source = %q, want verify", completion.Source)
	}
	if completion.Detail != string(statusBody) || completion.DetailTruncated {
		t.Errorf("completion detail = %q truncated=%v, want the status response body", completion.Detail, completion.DetailTruncated)
	}
	if !completion.OccurredAt.Equal(testNow()) {
		t.Errorf("completion occurred_at = %v, want the query time %v", completion.OccurredAt, testNow())
	}
	wantDedup := testSHA256(`["` + response.SubID + `","verify","succeeded"]`)
	if completion.DedupKey != wantDedup {
		t.Errorf("completion dedup_key = %q, want %q", completion.DedupKey, wantDedup)
	}
	if completion.EventID != `["`+response.SubID+`","succeeded"]` {
		t.Errorf("completion event_id = %q", completion.EventID)
	}

	if connector.calls != 1 {
		t.Errorf("status query calls = %d, want 1", connector.calls)
	}
	if connector.lastAPI != "extsim-job" {
		t.Errorf("status query api = %q, want extsim-job", connector.lastAPI)
	}
	if connector.lastCorrelation["job.id"] != "j-1" || connector.lastCorrelation["job.project"] != "demo" {
		t.Errorf("status query correlation = %v, want the subscription correlation", connector.lastCorrelation)
	}
}

func TestSubscribeStatusQueryUnreachableStaysActive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		connector *fakeConnector
	}{
		{
			name:      "not reached",
			connector: &fakeConnector{reached: false, body: []byte(`{"status":"running"}`)},
		},
		{
			name:      "unreachable",
			connector: &fakeConnector{err: errors.New("connection refused")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler, pool := activeTestEndpoint(t, tc.connector)
			ctx := t.Context()

			rec := doEndpointRequest(t, handler, http.MethodPost, "/v1/runs/run-1/subscriptions", validSubscribeRequest())
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
			}
			response := readSubscribeResponse(t, rec.Body.Bytes())
			if response.Status != "active" {
				t.Fatalf("status = %q, want active", response.Status)
			}

			status, terminal, _ := readTestSubscription(t, ctx, pool, response.SubID)
			if status != "active" || terminal != "" {
				t.Fatalf("subscription = status %q terminal %q, want active with no terminal", status, terminal)
			}
			if got := countTestRows(t, ctx, pool, "completion_delivery", response.SubID); got != 0 {
				t.Fatalf("completion deliveries = %d, want 0", got)
			}
		})
	}
}
