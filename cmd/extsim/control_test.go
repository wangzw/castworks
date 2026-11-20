package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestAssembly returns an assembly driven by a fake clock. Plans can be
// queued and jobs reach their planned duration only when the test advances the
// clock.
func newTestAssembly(t *testing.T) (*serviceAssembly, assemblyTestClock) {
	t.Helper()
	clock := newAssemblyTestClock()
	svc := NewJobService(clock, time.Minute)
	sender := newCallbackSender("http://127.0.0.1:1"+callbackWebhookPath, writeTestKey(t, "test-key\n"), clock, discardTestLogger())
	t.Cleanup(sender.close)
	return newServiceAssembly(svc, sender, discardTestLogger()), clock
}

// postControlPlan sends body to POST /control/plans.
func postControlPlan(t *testing.T, asm *serviceAssembly, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	asm.controlHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, controlPlansPath, strings.NewReader(body)))
	return rec
}

// submitJobHTTP sends body to POST /v1/jobs through the assembly handler.
func submitJobHTTP(t *testing.T, asm *serviceAssembly, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	asm.jobsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body)))
	return rec
}

// advanceAssemblyClock waits until the fake clock has n active timers and
// advances it by d.
func advanceAssemblyClock(t *testing.T, clock assemblyTestClock, n int, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clock.WaitForTimers(ctx, n); err != nil {
		t.Fatalf("waiting for %d active timer(s): %v", n, err)
	}
	clock.Advance(d)
}

// waitForAssemblyJob polls the service until the job reaches want.
func waitForAssemblyJob(t *testing.T, svc *JobService, id string, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, ok := svc.Get(id)
		if !ok {
			t.Fatalf("job %s is not stored", id)
		}
		if job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stayed %s, want %s", id, job.Status, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// decodeJob decodes a job response body.
func decodeJob(t *testing.T, body []byte) Job {
	t.Helper()
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return job
}

// TestControlPlanAppliesToHTTPSubmit checks that a plan queued through the
// control interface sets the duration and callback of a job submitted through
// the job interface.
func TestControlPlanAppliesToHTTPSubmit(t *testing.T) {
	asm, clock := newTestAssembly(t)

	rec := postControlPlan(t, asm, `{"duration_ms": 1500, "callback": "drop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /control/plans = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec = submitJobHTTP(t, asm, `{"project": "demo", "content": "{\"owner\": \"tester\"}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec.Body.Bytes())
	if job.Status != JobStatusQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if asm.pendingPlans() != 0 {
		t.Errorf("pending plans = %d after the submission, want 0", asm.pendingPlans())
	}

	plan, ok := asm.jobPlanFor(job.ID)
	if !ok {
		t.Fatalf("job %s has no attached plan", job.ID)
	}
	if plan.Duration == nil {
		t.Errorf("plan duration is unset, want 1500ms")
	} else if *plan.Duration != 1500*time.Millisecond {
		t.Errorf("plan duration = %s, want 1500ms", *plan.Duration)
	}
	if plan.Callback.Callback != callbackDrop {
		t.Errorf("plan callback = %q, want %q", plan.Callback.Callback, callbackDrop)
	}

	// The job must finish at the planned duration, not at the service default.
	advanceAssemblyClock(t, clock, 1, 1500*time.Millisecond)
	completed := waitForAssemblyJob(t, asm.svc, job.ID, JobStatusCompleted)
	if completed.Conclusion != JobConclusionSucceeded {
		t.Errorf("conclusion = %q, want succeeded", completed.Conclusion)
	}
}

// TestControlPlanExplicitZeroDuration checks that an explicit duration_ms of
// zero completes the job at once instead of falling back to --job-duration.
func TestControlPlanExplicitZeroDuration(t *testing.T) {
	// The service default is one minute, so a job that falls back to it cannot
	// complete while the fake clock stands still.
	asm, _ := newTestAssembly(t)

	rec := postControlPlan(t, asm, `{"duration_ms": 0, "callback": "drop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /control/plans = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec = submitJobHTTP(t, asm, `{"project": "demo", "content": "{\"owner\": \"tester\"}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec.Body.Bytes())

	// No clock advance: a zero duration is due immediately.
	completed := waitForAssemblyJob(t, asm.svc, job.ID, JobStatusCompleted)
	if completed.Conclusion != JobConclusionSucceeded {
		t.Errorf("conclusion = %q, want %q", completed.Conclusion, JobConclusionSucceeded)
	}
}

// TestControlPlanOmittedDurationUsesDefault checks that a plan that omits
// duration_ms still runs its job for --job-duration.
func TestControlPlanOmittedDurationUsesDefault(t *testing.T) {
	asm, clock := newTestAssembly(t)

	rec := postControlPlan(t, asm, `{"callback": "drop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /control/plans = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec = submitJobHTTP(t, asm, `{"project": "demo", "content": "{\"owner\": \"tester\"}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec.Body.Bytes())
	waitForAssemblyJob(t, asm.svc, job.ID, JobStatusRunning)

	advanceAssemblyClock(t, clock, 1, time.Minute-time.Second)
	if got, _ := asm.svc.Get(job.ID); got.Status != JobStatusRunning {
		t.Fatalf("job status before the default duration = %s, want %s", got.Status, JobStatusRunning)
	}
	clock.Advance(time.Second)

	if done := waitForAssemblyJob(t, asm.svc, job.ID, JobStatusCompleted); done.Conclusion != JobConclusionSucceeded {
		t.Errorf("conclusion = %q, want %q", done.Conclusion, JobConclusionSucceeded)
	}
}

// TestControlPlanCallbackDelayZero checks that an omitted callback_delay_ms and
// an explicit zero both mean no delay: unlike duration_ms, the delay has no
// startup default to fall back to.
func TestControlPlanCallbackDelayZero(t *testing.T) {
	asm, _ := newTestAssembly(t)

	if rec := postControlPlan(t, asm, `{"callback": "drop"}`); rec.Code != http.StatusCreated {
		t.Fatalf("plan with omitted delay = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := postControlPlan(t, asm, `{"callback": "drop", "callback_delay_ms": 0}`); rec.Code != http.StatusCreated {
		t.Fatalf("plan with explicit zero delay = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	omitted := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())
	explicit := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())

	for name, id := range map[string]string{"omitted": omitted.ID, "explicit zero": explicit.ID} {
		plan, ok := asm.jobPlanFor(id)
		if !ok {
			t.Fatalf("%s: job %s has no attached plan", name, id)
		}
		if plan.Callback.CallbackDelayMS != 0 {
			t.Errorf("%s: callback_delay_ms = %d, want 0", name, plan.Callback.CallbackDelayMS)
		}
	}
}

// TestControlRejectedSubmitKeepsPlan checks that a request rejected with 400
// leaves the next plan in place for a later, valid submission.
func TestControlRejectedSubmitKeepsPlan(t *testing.T) {
	asm, _ := newTestAssembly(t)

	rec := postControlPlan(t, asm, `{"duration_ms": 250, "callback": "drop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /control/plans = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	oversized, err := json.Marshal(map[string]string{
		"project": "demo",
		"content": strings.Repeat("a", maxJobContentBytes+1),
	})
	if err != nil {
		t.Fatalf("marshal oversized request: %v", err)
	}

	rejected := []struct {
		name string
		body string
	}{
		{name: "empty project", body: `{"project": "", "content": "{}"}`},
		{name: "oversized content", body: string(oversized)},
		{name: "malformed body", body: `{"project": `},
		{name: "trailing data", body: `{"project": "demo", "content": "{}"} {}`},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			rec := submitJobHTTP(t, asm, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /v1/jobs = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := asm.pendingPlans(); got != 1 {
				t.Fatalf("pending plans = %d after a rejected request, want 1", got)
			}
		})
	}

	rec = submitJobHTTP(t, asm, `{"project": "demo", "content": "{\"owner\": \"tester\"}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid POST /v1/jobs = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec.Body.Bytes())
	plan, ok := asm.jobPlanFor(job.ID)
	if !ok {
		t.Fatalf("job %s did not receive the preserved plan", job.ID)
	}
	if plan.Duration == nil {
		t.Errorf("plan duration is unset, want 250ms")
	} else if *plan.Duration != 250*time.Millisecond {
		t.Errorf("plan duration = %s, want 250ms", *plan.Duration)
	}
	if got := asm.pendingPlans(); got != 0 {
		t.Errorf("pending plans = %d after the valid submission, want 0", got)
	}
}

// TestControlPlansOrderAndDelete checks that plans are used in append order and
// that DELETE drops the unused ones.
func TestControlPlansOrderAndDelete(t *testing.T) {
	asm, _ := newTestAssembly(t)

	if rec := postControlPlan(t, asm, `{"duration_ms": 100, "callback": "drop"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first plan = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := postControlPlan(t, asm, `{"duration_ms": 200, "callback": "drop", "callback_delay_ms": 25}`); rec.Code != http.StatusCreated {
		t.Fatalf("second plan = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	first := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())
	second := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())
	third := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())

	firstPlan, ok := asm.jobPlanFor(first.ID)
	if !ok || firstPlan.Duration == nil || *firstPlan.Duration != 100*time.Millisecond {
		t.Errorf("first job plan = %+v (attached %v), want the 100ms plan", firstPlan, ok)
	}
	secondPlan, ok := asm.jobPlanFor(second.ID)
	if !ok {
		t.Fatalf("second job has no plan")
	}
	if secondPlan.Duration == nil || *secondPlan.Duration != 200*time.Millisecond || secondPlan.Callback.CallbackDelayMS != 25 {
		t.Errorf("second job plan = %+v, want the 200ms plan with a 25ms callback delay", secondPlan)
	}
	if _, ok := asm.jobPlanFor(third.ID); ok {
		t.Errorf("third job has a plan, want the default because the queue was empty")
	}

	rec := httptest.NewRecorder()
	asm.controlHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, controlPlansPath, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /control/plans = %d, want 204", rec.Code)
	}
	if got := asm.pendingPlans(); got != 0 {
		t.Fatalf("pending plans = %d after DELETE, want 0", got)
	}
	fourth := decodeJob(t, submitJobHTTP(t, asm, `{"project": "demo", "content": "{}"}`).Body.Bytes())
	if _, ok := asm.jobPlanFor(fourth.ID); ok {
		t.Errorf("a job submitted after DELETE has a plan, want the default")
	}
}

// TestControlPlanValidation checks the rejected plan requests.
func TestControlPlanValidation(t *testing.T) {
	asm, _ := newTestAssembly(t)

	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		want      int
		wantError string
	}{
		{name: "wrong method", method: http.MethodGet, path: controlPlansPath, want: http.StatusMethodNotAllowed},
		{name: "invalid JSON", method: http.MethodPost, path: controlPlansPath, body: "{", want: http.StatusBadRequest},
		{name: "unknown callback", method: http.MethodPost, path: controlPlansPath, body: `{"callback": "retry"}`, want: http.StatusBadRequest},
		{name: "negative duration", method: http.MethodPost, path: controlPlansPath, body: `{"duration_ms": -1}`, want: http.StatusBadRequest},
		{name: "negative delay", method: http.MethodPost, path: controlPlansPath, body: `{"callback_delay_ms": -1}`, want: http.StatusBadRequest, wantError: "callback_delay_ms must not be negative"},
		{name: "oversized duration", method: http.MethodPost, path: controlPlansPath, body: fmt.Sprintf(`{"duration_ms": %d}`, maxMilliseconds+1), want: http.StatusBadRequest, wantError: "duration_ms is too large"},
		{name: "oversized delay", method: http.MethodPost, path: controlPlansPath, body: fmt.Sprintf(`{"callback_delay_ms": %d}`, maxMilliseconds+1), want: http.StatusBadRequest, wantError: "callback_delay_ms is too large"},
		{name: "unknown path", method: http.MethodGet, path: "/control/other", want: http.StatusNotFound},
		{name: "delivery record wrong method", method: http.MethodPost, path: "/control/jobs/job-1/deliveries", want: http.StatusMethodNotAllowed},
		{name: "delivery record missing id", method: http.MethodGet, path: "/control/jobs//deliveries", want: http.StatusNotFound},
		{name: "delivery record wrong suffix", method: http.MethodGet, path: "/control/jobs/job-1/other", want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			asm.controlHandler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
			if tc.wantError != "" {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Errorf("decode error body: %v", err)
				} else if body.Error != tc.wantError {
					t.Errorf("error = %q, want %q", body.Error, tc.wantError)
				}
			}
			if tc.method == http.MethodPost && tc.path == controlPlansPath {
				if got := asm.pendingPlans(); got != 0 {
					t.Errorf("rejected plan left %d plans in the queue, want 0", got)
				}
			}
		})
	}
}

// TestControlDeliveries checks the delivery record response.
func TestControlDeliveries(t *testing.T) {
	asm, _ := newTestAssembly(t)

	first := callbackAttempt{DeliveryID: "delivery-1", Attempt: 1, SentAt: assemblyTestStart.Add(3 * time.Second), Error: "connection refused"}
	second := callbackAttempt{DeliveryID: "delivery-2", Attempt: 1, SentAt: assemblyTestStart.Add(4 * time.Second), StatusCode: http.StatusOK}
	asm.sender.record("job-recorded", first)
	asm.sender.record("job-recorded", second)

	rec := httptest.NewRecorder()
	asm.controlHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/jobs/job-recorded/deliveries", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery records = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp deliveriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode delivery records: %v", err)
	}
	if len(resp.Deliveries) != 2 {
		t.Fatalf("deliveries = %d, want 2: %s", len(resp.Deliveries), rec.Body.String())
	}
	if got, want := resp.Deliveries[0], (deliveryRecord{
		DeliveryID: "delivery-1",
		Attempt:    1,
		SentAt:     first.SentAt.Format(jobTimeFormat),
		Error:      "connection refused",
	}); got != want {
		t.Errorf("first record = %+v, want %+v", got, want)
	}
	if got, want := resp.Deliveries[1], (deliveryRecord{
		DeliveryID: "delivery-2",
		Attempt:    1,
		SentAt:     second.SentAt.Format(jobTimeFormat),
		StatusCode: http.StatusOK,
	}); got != want {
		t.Errorf("second record = %+v, want %+v", got, want)
	}

	rec = httptest.NewRecorder()
	asm.controlHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/jobs/job-unknown/deliveries", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown job delivery records = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"deliveries":[]`) {
		t.Errorf("unknown job body = %s, want an empty deliveries array", rec.Body.String())
	}
}

// TestExternalSystemDefinitionMatchesConstants checks that the checked-in
// definition uses the same webhook path, delivery header and event type as the
// extsim constants.
func TestExternalSystemDefinitionMatchesConstants(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "dev", "connectorhub", "extsim.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read external system definition: %v", err)
	}
	contents := string(data)

	for _, want := range []string{
		"kind: ExternalSystem",
		"name: extsim",
		"path: " + callbackWebhookPath,
		"delivery_id: " + callbackDeliveryHeader,
		"event_types: [" + callbackEventCompleted + "]",
		"signature: hmac-sha256",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("definition does not contain %q:\n%s", want, contents)
		}
	}
}
