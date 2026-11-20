package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Paths of the test-control interface, from docs/milestone/v0.2.0/extsim.md
// "测试控制接口".
const (
	controlPlansPath   = "/control/plans"
	controlJobsPrefix  = "/control/jobs/"
	controlDeliveries  = "deliveries"
	maxControlBodySize = 64 * 1024
)

// maxMilliseconds bounds a millisecond value accepted from the control
// interface so that converting it to a time.Duration cannot overflow.
const maxMilliseconds = int64(math.MaxInt64) / int64(time.Millisecond)

// jobPlan is one queued plan: the job duration plus how its callback is sent.
// Duration is nil when the request omitted duration_ms, meaning the service's
// default duration; a non-nil zero means the job completes at once.
type jobPlan struct {
	Duration *time.Duration
	Callback callbackPlan
}

// planRequest is the JSON body of POST /control/plans. Every field may be
// omitted. DurationMS is a pointer so that an omitted field can be told apart
// from an explicit zero: the former selects --job-duration, the latter makes
// the job complete immediately. CallbackDelayMS has no startup default, so
// omitting it and giving zero both mean no delay.
type planRequest struct {
	DurationMS      *int64 `json:"duration_ms"`
	Callback        string `json:"callback"`
	CallbackDelayMS int    `json:"callback_delay_ms"`
}

// planJSON is the JSON form returned by POST /control/plans.
type planJSON struct {
	DurationMS      int64        `json:"duration_ms"`
	Callback        callbackKind `json:"callback"`
	CallbackDelayMS int          `json:"callback_delay_ms"`
}

// deliveryRecord is one callback attempt as returned by the delivery record
// interface.
type deliveryRecord struct {
	DeliveryID string `json:"delivery_id"`
	Attempt    int    `json:"attempt"`
	SentAt     string `json:"sent_at"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// deliveriesResponse is the body of GET /control/jobs/{id}/deliveries.
type deliveriesResponse struct {
	Deliveries []deliveryRecord `json:"deliveries"`
}

// enqueuePlan appends a plan to the queue.
func (a *serviceAssembly) enqueuePlan(plan jobPlan) {
	a.plansMu.Lock()
	defer a.plansMu.Unlock()
	a.plans = append(a.plans, plan)
}

// popPlan returns the next plan, or nil when the queue is empty.
func (a *serviceAssembly) popPlan() *jobPlan {
	a.plansMu.Lock()
	defer a.plansMu.Unlock()
	if len(a.plans) == 0 {
		return nil
	}
	plan := a.plans[0]
	a.plans = a.plans[1:]
	return &plan
}

// clearPlans drops every unused plan.
func (a *serviceAssembly) clearPlans() {
	a.plansMu.Lock()
	defer a.plansMu.Unlock()
	a.plans = nil
}

// pendingPlans returns the number of unused plans. It exists for tests.
func (a *serviceAssembly) pendingPlans() int {
	a.plansMu.Lock()
	defer a.plansMu.Unlock()
	return len(a.plans)
}

// controlHandler serves the test-control interface.
func (a *serviceAssembly) controlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == controlPlansPath:
			a.handlePlans(w, r)
		case strings.HasPrefix(r.URL.Path, controlJobsPrefix):
			a.handleDeliveries(w, r)
		default:
			writeJobError(w, http.StatusNotFound, "not found")
		}
	})
}

// handlePlans serves POST and DELETE /control/plans.
func (a *serviceAssembly) handlePlans(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.appendPlan(w, r)
	case http.MethodDelete:
		a.clearPlans()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJobError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// appendPlan serves POST /control/plans.
func (a *serviceAssembly) appendPlan(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBodySize)
	var req planRequest
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

	if req.DurationMS != nil {
		if err := validateMilliseconds("duration_ms", *req.DurationMS); err != nil {
			writeJobError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := validateMilliseconds("callback_delay_ms", int64(req.CallbackDelayMS)); err != nil {
		writeJobError(w, http.StatusBadRequest, err.Error())
		return
	}
	callback, err := newCallbackPlan(req.Callback, req.CallbackDelayMS)
	if err != nil {
		writeJobError(w, http.StatusBadRequest, err.Error())
		return
	}

	var durationMS int64
	plan := jobPlan{Callback: callback}
	if req.DurationMS != nil {
		durationMS = *req.DurationMS
		duration := time.Duration(durationMS) * time.Millisecond
		plan.Duration = &duration
	}
	a.enqueuePlan(plan)
	writeJobJSON(w, http.StatusCreated, planJSON{
		DurationMS:      durationMS,
		Callback:        plan.Callback.Callback,
		CallbackDelayMS: plan.Callback.CallbackDelayMS,
	})
}

// validateMilliseconds rejects negative and overflowing millisecond values.
func validateMilliseconds(name string, value int64) error {
	if value < 0 {
		return fmt.Errorf("%s must not be negative", name)
	}
	if value > maxMilliseconds {
		return fmt.Errorf("%s is too large", name)
	}
	return nil
}

// handleDeliveries serves GET /control/jobs/{id}/deliveries.
func (a *serviceAssembly) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJobError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, suffix, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, controlJobsPrefix), "/")
	if !ok || id == "" || suffix != controlDeliveries {
		writeJobError(w, http.StatusNotFound, "not found")
		return
	}

	attempts := a.sender.attempts(id)
	records := make([]deliveryRecord, 0, len(attempts))
	for _, attempt := range attempts {
		records = append(records, deliveryRecord{
			DeliveryID: attempt.DeliveryID,
			Attempt:    attempt.Attempt,
			SentAt:     attempt.SentAt.Format(jobTimeFormat),
			StatusCode: attempt.StatusCode,
			Error:      attempt.Error,
		})
	}
	writeJobJSON(w, http.StatusOK, deliveriesResponse{Deliveries: records})
}
