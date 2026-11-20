package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Clock is the time source used by the extsim services. Tests use
// internal/testutil/fakeclock through a small adapter so that job durations
// never have to be waited for.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a single-shot timer created by a Clock.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// RealClock returns a Clock backed by the system time.
func RealClock() Clock {
	return realClock{}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{timer: time.NewTimer(d)}
}

type realTimer struct {
	timer *time.Timer
}

func (t realTimer) C() <-chan time.Time { return t.timer.C }

func (t realTimer) Stop() bool { return t.timer.Stop() }

// JobStatus is the lifecycle state of a job.
type JobStatus string

// The job lifecycle states. A job starts queued, moves to running at once and
// completes after its duration.
const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
)

// Job conclusions, set when a job completes.
const (
	JobConclusionSucceeded = "succeeded"
	JobConclusionFailed    = "failed"
)

// Reasons a job may have failed. The conclusion depends only on the submitted
// content; the two messages are fixed by the milestone document.
const (
	jobReasonNotObject  = "content is not a JSON object"
	jobReasonMissingOwn = `missing required field "owner"`
)

// maxJobContentBytes is the largest accepted job content.
const maxJobContentBytes = 64 * 1024

// ErrJobProjectEmpty is returned by Submit when the project is empty.
var ErrJobProjectEmpty = errors.New("project must not be empty")

// ErrJobContentTooLarge is returned by Submit when the content exceeds
// maxJobContentBytes.
var ErrJobContentTooLarge = errors.New("content must not exceed 65536 bytes")

// Job is one job accepted by the job interface. Its JSON form is the job
// object described by the milestone document; it never contains the submitted
// content.
type Job struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Status     JobStatus `json:"status"`
	Conclusion string    `json:"conclusion"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// content is the submitted content. The conclusion is derived from it when
	// the job completes.
	content string
}

// jobTimeFormat renders a time as RFC 3339 with exactly three fractional
// digits, as the milestone document requires millisecond precision.
const jobTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// jobJSON is the wire form of Job. MarshalJSON builds it directly so that
// timestamps always carry exactly three fractional digits, which time.Time's
// own marshaller does not guarantee.
type jobJSON struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Status     JobStatus `json:"status"`
	Conclusion string    `json:"conclusion"`
	Reason     string    `json:"reason"`
	CreatedAt  string    `json:"created_at"`
	UpdatedAt  string    `json:"updated_at"`
}

// MarshalJSON implements json.Marshaler.
func (j Job) MarshalJSON() ([]byte, error) {
	return json.Marshal(jobJSON{
		ID:         j.ID,
		Project:    j.Project,
		Status:     j.Status,
		Conclusion: j.Conclusion,
		Reason:     j.Reason,
		CreatedAt:  j.CreatedAt.Format(jobTimeFormat),
		UpdatedAt:  j.UpdatedAt.Format(jobTimeFormat),
	})
}

// JobService accepts jobs, runs them on its clock and serves the job HTTP
// interface.
//
// Submit accepts an optional duration, zero meaning the service's default
// duration. Every status change is reported to the status-change hook; a job
// reaching completed is also passed to the completion hook, which the service
// assembly uses to drive callbacks.
type JobService struct {
	clock           Clock
	defaultDuration time.Duration

	mu             sync.Mutex
	jobs           map[string]Job
	onStatusChange func(Job)
	onComplete     func(Job)
}

// NewJobService returns a JobService that runs each job for defaultDuration
// unless the submitter asks for another duration.
func NewJobService(clock Clock, defaultDuration time.Duration) *JobService {
	if clock == nil {
		clock = RealClock()
	}
	return &JobService{
		clock:           clock,
		defaultDuration: defaultDuration,
		jobs:            make(map[string]Job),
	}
}

// SetOnStatusChange registers a hook called after every job status change,
// including the initial queued state. The hook runs outside the service lock
// and must not block. Register it before serving.
func (s *JobService) SetOnStatusChange(fn func(Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onStatusChange = fn
}

// SetOnComplete registers a hook called once a job completes, after its status
// change has been recorded. The hook runs outside the service lock and must not
// block. Register it before serving.
func (s *JobService) SetOnComplete(fn func(Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onComplete = fn
}

// Submit validates the job input, records a queued job and starts it. A zero
// duration selects the service's default duration. The returned job is the
// queued snapshot; the job moves to running immediately afterwards.
func (s *JobService) Submit(project, content string, duration time.Duration) (Job, error) {
	if project == "" {
		return Job{}, ErrJobProjectEmpty
	}
	if len(content) > maxJobContentBytes {
		return Job{}, ErrJobContentTooLarge
	}
	if duration == 0 {
		duration = s.defaultDuration
	}

	now := s.clock.Now()
	job := Job{
		Project:   project,
		Status:    JobStatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
		content:   content,
	}

	s.mu.Lock()
	for {
		id, err := newJobID()
		if err != nil {
			s.mu.Unlock()
			return Job{}, err
		}
		if _, exists := s.jobs[id]; exists {
			continue
		}
		job.ID = id
		s.jobs[id] = job
		break
	}
	onStatusChange := s.onStatusChange
	s.mu.Unlock()

	notifyJobHook(onStatusChange, job)
	go s.run(job.ID, duration)
	return job, nil
}

// Get returns the job with the given id. The second result reports whether the
// job exists.
func (s *JobService) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

// run moves a job to running, waits for its duration and completes it.
func (s *JobService) run(id string, duration time.Duration) {
	s.transition(id, JobStatusRunning, "", "")

	timer := s.clock.NewTimer(duration)
	<-timer.C()
	timer.Stop()

	job, ok := s.Get(id)
	if !ok {
		return
	}
	conclusion, reason := jobConclusion(job.content)
	job = s.transition(id, JobStatusCompleted, conclusion, reason)

	s.mu.Lock()
	onComplete := s.onComplete
	s.mu.Unlock()
	notifyJobHook(onComplete, job)
}

// transition records a status change and reports it to the status-change hook.
// It returns the updated job.
func (s *JobService) transition(id string, status JobStatus, conclusion, reason string) Job {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return Job{}
	}
	job.Status = status
	job.Conclusion = conclusion
	job.Reason = reason
	job.UpdatedAt = s.clock.Now()
	s.jobs[id] = job
	onStatusChange := s.onStatusChange
	s.mu.Unlock()

	notifyJobHook(onStatusChange, job)
	return job
}

// notifyJobHook calls hook with job unless hook is nil.
func notifyJobHook(hook func(Job), job Job) {
	if hook != nil {
		hook(job)
	}
}

// jobConclusion applies the fixed validation rules to the submitted content.
func jobConclusion(content string) (conclusion, reason string) {
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return JobConclusionFailed, jobReasonNotObject
	}
	object, ok := value.(map[string]any)
	if !ok {
		return JobConclusionFailed, jobReasonNotObject
	}
	owner, ok := object["owner"].(string)
	if !ok || owner == "" {
		return JobConclusionFailed, jobReasonMissingOwn
	}
	return JobConclusionSucceeded, ""
}

// newJobID returns a job identifier: "job-" followed by 12 lowercase base32
// characters.
func newJobID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return "job-" + strings.ToLower(encoded)[:12], nil
}

// maxJobBodyBytes bounds the request body read by the job interface. It leaves
// room for a maximum-size content whose characters need JSON escaping.
const maxJobBodyBytes = 1 << 20

// Handler returns the job HTTP interface: POST /v1/jobs and
// GET /v1/jobs/{id}.
func (s *JobService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/jobs":
			if r.Method != http.MethodPost {
				writeJobError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			s.handleSubmit(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
			if id == "" || strings.Contains(id, "/") {
				writeJobError(w, http.StatusNotFound, "job not found")
				return
			}
			if r.Method != http.MethodGet {
				writeJobError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			s.handleGet(w, id)
		default:
			writeJobError(w, http.StatusNotFound, "not found")
		}
	})
}

// handleSubmit serves POST /v1/jobs.
func (s *JobService) handleSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJobBodyBytes)
	var req struct {
		Project string `json:"project"`
		Content string `json:"content"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJobError(w, http.StatusBadRequest, "request body too large")
			return
		}
		writeJobError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJobError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	job, err := s.Submit(req.Project, req.Content, 0)
	switch {
	case err == nil:
		writeJobJSON(w, http.StatusCreated, job)
	case errors.Is(err, ErrJobProjectEmpty), errors.Is(err, ErrJobContentTooLarge):
		writeJobError(w, http.StatusBadRequest, err.Error())
	default:
		writeJobError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleGet serves GET /v1/jobs/{id}.
func (s *JobService) handleGet(w http.ResponseWriter, id string) {
	job, ok := s.Get(id)
	if !ok {
		writeJobError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJobJSON(w, http.StatusOK, job)
}

// writeJobJSON writes v as a JSON response with the given status.
func writeJobJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJobError writes the fixed error body {"error": message}.
func writeJobError(w http.ResponseWriter, status int, message string) {
	writeJobJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}
