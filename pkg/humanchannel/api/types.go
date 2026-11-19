// Package api defines the JSON request and response bodies of the
// humanchannel HTTP API.
package api

import (
	"encoding/json"
	"time"
)

// timeLayout is the wire layout for Time: UTC with exactly three fractional
// digits. The trailing Z is a literal; the timestamp is already converted to
// UTC before formatting.
const timeLayout = "2006-01-02T15:04:05.000Z"

// Time is a time.Time that marshals as UTC RFC 3339 with fixed millisecond
// precision. Unmarshalling accepts any valid RFC 3339 timestamp, including
// zone offsets and other fractional-second widths.
type Time struct{ time.Time }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Truncate(time.Millisecond).Format(timeLayout))
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// CreateRunRequest is the body of POST /v1/runs.
type CreateRunRequest struct {
	Prompt string `json:"prompt"`
}

// CreateRunResponse is the response body of POST /v1/runs.
type CreateRunResponse struct {
	RunID string `json:"run_id"`
}

// RunSummary is one element of GET /v1/runs and the shared prefix of
// RunDetail.
type RunSummary struct {
	RunID     string `json:"run_id"`
	State     string `json:"state"`
	Substate  string `json:"substate"`
	CreatedAt Time   `json:"created_at"`
}

// Turn is one agent turn of a run detail.
type Turn struct {
	TurnID    string `json:"turn_id"`
	StartedAt Time   `json:"started_at"`
	EndedAt   *Time  `json:"ended_at"`
	Output    string `json:"output"`
	IsError   bool   `json:"is_error"`
}

// Suspension is one suspend/resume record of a run detail.
type Suspension struct {
	StartedAt  Time   `json:"started_at"`
	SnapshotMS *int64 `json:"snapshot_ms"`
	RestoreMS  *int64 `json:"restore_ms"`
	HelloMS    *int64 `json:"hello_ms"`
	Error      string `json:"error"`
}

// RunDetail is the response body of GET /v1/runs/{run_id}.
type RunDetail struct {
	RunSummary
	FailureReason string       `json:"failure_reason"`
	Turns         []Turn       `json:"turns"`
	Suspensions   []Suspension `json:"suspensions"`
}

// SendMessageRequest is the body of POST /v1/runs/{run_id}/messages.
type SendMessageRequest struct {
	Text string `json:"text"`
}

// SendMessageResponse is the response body of
// POST /v1/runs/{run_id}/messages.
type SendMessageResponse struct {
	InboxSeq int64 `json:"inbox_seq"`
}

// ErrorResponse is the body of an error response.
type ErrorResponse struct {
	Error string `json:"error"`
}
