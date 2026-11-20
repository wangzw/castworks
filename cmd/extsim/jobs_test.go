package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
)

// jobFakeClock adapts a fakeclock.Clock to the Clock interface. The adapter is
// needed because fakeclock.NewTimer returns runcontroller.Timer while Clock
// names its own Timer interface; both have the same method set.
type jobFakeClock struct {
	clock *fakeclock.Clock
}

func (c jobFakeClock) Now() time.Time { return c.clock.Now() }

func (c jobFakeClock) NewTimer(d time.Duration) Timer { return c.clock.NewTimer(d) }

// jobTestStart is a fixed clock start with a nanosecond part, so the tests can
// check that timestamps are rendered at millisecond precision.
var jobTestStart = time.Date(2026, time.November, 20, 9, 0, 0, 123456789, time.UTC)

// newJobTestService returns a service driven by a fake clock.
func newJobTestService(t *testing.T, defaultDuration time.Duration) (*JobService, *fakeclock.Clock) {
	t.Helper()
	clk := fakeclock.New(jobTestStart)
	return NewJobService(jobFakeClock{clock: clk}, defaultDuration), clk
}

// jobWaitForStatus polls the service until the job reaches want.
func jobWaitForStatus(t *testing.T, svc *JobService, id string, want JobStatus) Job {
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

// jobAdvance waits until the fake clock has n active timers and then advances
// it by d, firing every timer that falls due.
func jobAdvance(t *testing.T, clk *fakeclock.Clock, n int, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clk.WaitForTimers(ctx, n); err != nil {
		t.Fatalf("wait for %d active timer(s): %v", n, err)
	}
	clk.Advance(d)
}

// jobJSONString marshals the job and returns one JSON string field.
func jobJSONString(t *testing.T, job Job, field string) string {
	t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal job JSON: %v", err)
	}
	value, ok := decoded[field]
	if !ok {
		t.Fatalf("job JSON has no field %q: %s", field, raw)
	}
	var str string
	if err := json.Unmarshal(value, &str); err != nil {
		t.Fatalf("field %q is not a string: %s", field, value)
	}
	return str
}

// TestJobValidation checks the fixed validation rules: the conclusion and
// reason for content that is not a JSON object, that lacks owner, that has an
// empty owner, and that is valid.
func TestJobValidation(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		conclusion string
		reason     string
	}{
		// The reasons are spelled out as the milestone document writes them, so
		// a change to the wording is caught here rather than following a
		// constant.
		{"not json", `not json`, JobConclusionFailed, "content is not a JSON object"},
		{"json array", `["owner"]`, JobConclusionFailed, "content is not a JSON object"},
		{"json null", `null`, JobConclusionFailed, "content is not a JSON object"},
		{"missing owner", `{"project":"demo"}`, JobConclusionFailed, `missing required field "owner"`},
		{"empty owner", `{"owner":""}`, JobConclusionFailed, `missing required field "owner"`},
		{"owner not a string", `{"owner":42}`, JobConclusionFailed, `missing required field "owner"`},
		{"valid", `{"owner":"alice","note":1}`, JobConclusionSucceeded, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const duration = 5 * time.Second
			svc, clk := newJobTestService(t, time.Minute)

			job, err := svc.Submit("demo", tc.content, duration)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if job.Status != JobStatusQueued {
				t.Fatalf("submitted job status = %s, want %s", job.Status, JobStatusQueued)
			}
			if job.Conclusion != "" || job.Reason != "" {
				t.Fatalf("queued job has conclusion %q and reason %q, want both empty", job.Conclusion, job.Reason)
			}

			jobAdvance(t, clk, 1, duration)
			done := jobWaitForStatus(t, svc, job.ID, JobStatusCompleted)
			if done.Conclusion != tc.conclusion {
				t.Errorf("conclusion = %q, want %q", done.Conclusion, tc.conclusion)
			}
			if done.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", done.Reason, tc.reason)
			}
		})
	}
}

// TestJobLifecycle checks queued -> running -> completed on a controllable
// clock, that the job completes only after its duration, and that timestamps
// are RFC 3339 with millisecond precision.
func TestJobLifecycle(t *testing.T) {
	const duration = 2 * time.Second
	svc, clk := newJobTestService(t, time.Minute)

	job, err := svc.Submit("demo", `{"owner":"alice"}`, duration)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.Status != JobStatusQueued {
		t.Fatalf("submitted status = %s, want %s", job.Status, JobStatusQueued)
	}
	if got, want := jobJSONString(t, job, "created_at"), "2026-11-20T09:00:00.123Z"; got != want {
		t.Errorf("created_at = %q, want %q", got, want)
	}
	if got, want := jobJSONString(t, job, "updated_at"), "2026-11-20T09:00:00.123Z"; got != want {
		t.Errorf("updated_at = %q, want %q", got, want)
	}

	jobWaitForStatus(t, svc, job.ID, JobStatusRunning)

	jobAdvance(t, clk, 1, time.Second)
	if got, _ := svc.Get(job.ID); got.Status != JobStatusRunning {
		t.Fatalf("job status after 1s = %s, want %s", got.Status, JobStatusRunning)
	}
	clk.Advance(time.Second)

	done := jobWaitForStatus(t, svc, job.ID, JobStatusCompleted)
	if got, want := jobJSONString(t, done, "updated_at"), "2026-11-20T09:00:02.123Z"; got != want {
		t.Errorf("updated_at = %q, want %q", got, want)
	}
	if got, want := jobJSONString(t, done, "status"), string(JobStatusCompleted); got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if done.Conclusion != JobConclusionSucceeded {
		t.Errorf("conclusion = %q, want %q", done.Conclusion, JobConclusionSucceeded)
	}
	if done.Reason != "" {
		t.Errorf("reason = %q, want empty", done.Reason)
	}

	// Both timestamps must be RFC 3339 with exactly three fractional digits.
	layout := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}(Z|[+-]\d{2}:\d{2})$`)
	for _, field := range []string{"created_at", "updated_at"} {
		value := jobJSONString(t, done, field)
		if !layout.MatchString(value) {
			t.Errorf("%s = %q, want RFC 3339 with millisecond precision", field, value)
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			t.Errorf("%s = %q is not RFC 3339: %v", field, value, err)
		}
	}
}

// TestJobDefaultDuration checks that a zero submit duration selects the
// service default and that the job runs for exactly that long.
func TestJobDefaultDuration(t *testing.T) {
	const defaultDuration = 10 * time.Second
	svc, clk := newJobTestService(t, defaultDuration)

	job, err := svc.Submit("demo", `{"owner":"alice"}`, 0)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	jobWaitForStatus(t, svc, job.ID, JobStatusRunning)

	jobAdvance(t, clk, 1, defaultDuration-time.Second)
	if got, _ := svc.Get(job.ID); got.Status != JobStatusRunning {
		t.Fatalf("job status before the default duration = %s, want %s", got.Status, JobStatusRunning)
	}
	clk.Advance(time.Second)
	if done := jobWaitForStatus(t, svc, job.ID, JobStatusCompleted); done.Conclusion != JobConclusionSucceeded {
		t.Errorf("conclusion = %q, want %q", done.Conclusion, JobConclusionSucceeded)
	}
}

// TestJobHooks checks that the status-change hook sees queued, running and
// completed in order and that the completion hook receives the completed job.
func TestJobHooks(t *testing.T) {
	const duration = 3 * time.Second
	svc, clk := newJobTestService(t, time.Minute)

	changes := make(chan JobStatus, 8)
	completions := make(chan Job, 1)
	svc.SetOnStatusChange(func(job Job) { changes <- job.Status })
	svc.SetOnComplete(func(job Job) { completions <- job })

	job, err := svc.Submit("demo", `{"owner":"alice"}`, duration)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	jobWaitForStatus(t, svc, job.ID, JobStatusRunning)
	jobAdvance(t, clk, 1, duration)
	jobWaitForStatus(t, svc, job.ID, JobStatusCompleted)

	want := []JobStatus{JobStatusQueued, JobStatusRunning, JobStatusCompleted}
	for i, status := range want {
		select {
		case got := <-changes:
			if got != status {
				t.Fatalf("status change %d = %s, want %s", i, got, status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for status change %d (%s)", i, status)
		}
	}

	select {
	case done := <-completions:
		if done.ID != job.ID || done.Status != JobStatusCompleted {
			t.Errorf("completion hook got %+v, want completed job %s", done, job.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the completion hook")
	}
}

// TestJobIDFormat checks the identifier format and uniqueness.
func TestJobIDFormat(t *testing.T) {
	svc, _ := newJobTestService(t, time.Hour)
	pattern := regexp.MustCompile(`^job-[a-z2-7]{12}$`)

	seen := make(map[string]bool)
	for i := 0; i < 8; i++ {
		job, err := svc.Submit("demo", `{"owner":"alice"}`, time.Hour)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if !pattern.MatchString(job.ID) {
			t.Fatalf("job id = %q, want %s", job.ID, pattern)
		}
		if seen[job.ID] {
			t.Fatalf("duplicate job id %q", job.ID)
		}
		seen[job.ID] = true
	}
}

// jobTestServer starts the job HTTP interface on a test server.
func jobTestServer(t *testing.T, svc *JobService) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(svc.Handler())
	t.Cleanup(server.Close)
	return server
}

// jobTestPost sends body to the server's POST /v1/jobs.
func jobTestPost(t *testing.T, server *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(server.URL+"/v1/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/jobs: %v", err)
	}
	return resp
}

// jobTestDecode decodes a JSON response body into v and closes it.
func jobTestDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if got, want := resp.Header.Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// TestJobHTTP checks the two endpoints, the 201 response, the 404 body and the
// 400 cases for an empty project and oversized content.
func TestJobHTTP(t *testing.T) {
	t.Run("submit", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		resp := jobTestPost(t, server, `{"project":"demo","content":"{\"owner\":\"alice\"}"}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
		}
		var got struct {
			ID         string `json:"id"`
			Project    string `json:"project"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Reason     string `json:"reason"`
			CreatedAt  string `json:"created_at"`
			UpdatedAt  string `json:"updated_at"`
		}
		jobTestDecode(t, resp, &got)
		if got.Project != "demo" || got.Status != string(JobStatusQueued) {
			t.Errorf("job = project %q status %q, want demo/queued", got.Project, got.Status)
		}
		if !regexp.MustCompile(`^job-[a-z2-7]{12}$`).MatchString(got.ID) {
			t.Errorf("id = %q, want job- plus 12 lowercase base32 characters", got.ID)
		}
		if got.Conclusion != "" || got.Reason != "" {
			t.Errorf("queued job has conclusion %q and reason %q, want both empty", got.Conclusion, got.Reason)
		}
		if got.CreatedAt == "" || got.UpdatedAt == "" {
			t.Errorf("timestamps are empty: %q, %q", got.CreatedAt, got.UpdatedAt)
		}

		getResp, err := http.Get(server.URL + "/v1/jobs/" + got.ID)
		if err != nil {
			t.Fatalf("GET job: %v", err)
		}
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d, want %d", getResp.StatusCode, http.StatusOK)
		}
		var fetched struct {
			ID string `json:"id"`
		}
		jobTestDecode(t, getResp, &fetched)
		if fetched.ID != got.ID {
			t.Errorf("GET id = %q, want %q", fetched.ID, got.ID)
		}
	})

	t.Run("empty project", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		resp := jobTestPost(t, server, `{"project":"","content":"{}"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
		var body struct {
			Error string `json:"error"`
		}
		jobTestDecode(t, resp, &body)
		if body.Error == "" {
			t.Error("error body is empty")
		}
	})

	t.Run("content exceeds 64 KiB", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		payload, err := json.Marshal(map[string]string{
			"project": "demo",
			"content": strings.Repeat("a", maxJobContentBytes+1),
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		resp := jobTestPost(t, server, string(payload))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
		var body struct {
			Error string `json:"error"`
		}
		jobTestDecode(t, resp, &body)
		if !strings.Contains(body.Error, "65536") {
			t.Errorf("error = %q, want it to mention the 65536-byte limit", body.Error)
		}
	})

	t.Run("content at the 64 KiB limit", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		payload, err := json.Marshal(map[string]string{
			"project": "demo",
			"content": strings.Repeat("a", maxJobContentBytes),
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		resp := jobTestPost(t, server, string(payload))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})

	t.Run("invalid JSON body", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		resp := jobTestPost(t, server, `not json`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
		var body struct {
			Error string `json:"error"`
		}
		jobTestDecode(t, resp, &body)
		if body.Error == "" {
			t.Error("error body is empty")
		}
	})

	t.Run("unknown job", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		resp, err := http.Get(server.URL + "/v1/jobs/job-aaaaaaaaaaaa")
		if err != nil {
			t.Fatalf("GET job: %v", err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
		var body struct {
			Error string `json:"error"`
		}
		jobTestDecode(t, resp, &body)
		if body.Error != "job not found" {
			t.Errorf("error = %q, want %q", body.Error, "job not found")
		}
	})

	t.Run("methods", func(t *testing.T) {
		svc, _ := newJobTestService(t, time.Hour)
		server := jobTestServer(t, svc)

		req, err := http.NewRequest(http.MethodDelete, server.URL+"/v1/jobs", bytes.NewReader(nil))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /v1/jobs: %v", err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
		}
		var body struct {
			Error string `json:"error"`
		}
		jobTestDecode(t, resp, &body)
		if body.Error == "" {
			t.Error("error body is empty")
		}
	})
}
