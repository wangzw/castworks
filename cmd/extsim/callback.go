package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// Callback protocol constants, fixed by docs/milestone/v0.2.0/extsim.md and
// deploy/dev/connectorhub/extsim.yaml. The control plane verifies the same
// header names and event type.
const (
	callbackEventCompleted  = "job.completed"
	callbackEventHeader     = "X-Extsim-Event"
	callbackDeliveryHeader  = "X-Extsim-Delivery"
	callbackSignatureHeader = "X-Extsim-Signature-256"

	// callbackInitialBackoff and callbackMaxBackoff bound the retry interval:
	// one second, doubling after each failed attempt, capped at thirty
	// seconds. The cap is reached after five waits.
	callbackInitialBackoff = time.Second
	callbackMaxBackoff     = 30 * time.Second

	// callbackHTTPTimeout bounds one callback request. Retries are governed by
	// the backoff above, so a stuck receiver must eventually fail the attempt.
	callbackHTTPTimeout = 10 * time.Second
)

// callbackKind is how a job's callback is delivered.
type callbackKind string

const (
	// callbackDeliver posts the callback and retries until a 2xx response.
	callbackDeliver callbackKind = "deliver"

	// callbackDrop sends nothing; the job still completes and stays visible
	// through the jobs API.
	callbackDrop callbackKind = "drop"

	// callbackDuplicate makes one normal delivery, then, after it receives its
	// first 2xx, posts the same body again under a new delivery identifier.
	callbackDuplicate callbackKind = "duplicate"
)

// callbackPlan is the callback part of a job plan. The jobs side keeps the
// job duration; this type only controls how the callback is sent.
type callbackPlan struct {
	Callback        callbackKind `json:"callback"`
	CallbackDelayMS int          `json:"callback_delay_ms"`
}

// newCallbackPlan validates the JSON-level values of a plan. An empty callback
// means "deliver"; a negative delay is rejected.
func newCallbackPlan(callback string, delayMS int) (callbackPlan, error) {
	if delayMS < 0 {
		return callbackPlan{}, fmt.Errorf("callback_delay_ms must not be negative")
	}
	if callback == "" {
		return callbackPlan{Callback: callbackDeliver, CallbackDelayMS: delayMS}, nil
	}
	switch kind := callbackKind(callback); kind {
	case callbackDeliver, callbackDrop, callbackDuplicate:
		return callbackPlan{Callback: kind, CallbackDelayMS: delayMS}, nil
	default:
		return callbackPlan{}, fmt.Errorf("callback must be one of %q, %q or %q, got %q",
			callbackDeliver, callbackDrop, callbackDuplicate, callback)
	}
}

// callbackAttempt is one delivery attempt as recorded for the control API.
type callbackAttempt struct {
	DeliveryID string    `json:"delivery_id"`
	Attempt    int       `json:"attempt"`
	SentAt     time.Time `json:"sent_at"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// callbackSender delivers one job's callback asynchronously and records every
// attempt. It does not know the job type: the caller passes the job identifier
// and the already encoded request body.
type callbackSender struct {
	callbackURL    string
	signingKeyFile string
	clock          Clock
	logger         *slog.Logger
	client         *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards closed and deliveries. Wrapping the WaitGroup's Add in mu
	// keeps it from racing with a Wait started after close.
	mu         sync.Mutex
	closed     bool
	deliveries map[string][]callbackAttempt
}

// newCallbackSender returns a sender posting to callbackURL. A nil clock means
// the system clock; a nil logger means slog.Default.
func newCallbackSender(callbackURL, signingKeyFile string, clock Clock, logger *slog.Logger) *callbackSender {
	if clock == nil {
		clock = RealClock()
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &callbackSender{
		callbackURL:    callbackURL,
		signingKeyFile: signingKeyFile,
		clock:          clock,
		logger:         logger,
		client:         &http.Client{Timeout: callbackHTTPTimeout},
		ctx:            ctx,
		cancel:         cancel,
		deliveries:     make(map[string][]callbackAttempt),
	}
}

// send starts delivery of body for jobID. It returns at once; the delivery
// runs in the background until it succeeds, is dropped, or the sender closes.
func (s *callbackSender) send(jobID string, body []byte, plan callbackPlan) {
	if plan.Callback == callbackDrop {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		if plan.Callback == callbackDuplicate {
			if s.deliver(jobID, body, plan.CallbackDelayMS) {
				s.deliver(jobID, body, 0)
			}
			return
		}
		s.deliver(jobID, body, plan.CallbackDelayMS)
	}()
}

// deliver sends body under one delivery identifier until a 2xx response or
// the sender closes, and reports whether a 2xx arrived. delay postpones the
// first attempt only.
func (s *callbackSender) deliver(jobID string, body []byte, delayMS int) bool {
	if !s.sleep(time.Duration(delayMS) * time.Millisecond) {
		return false
	}
	deliveryID := newCallbackDeliveryID()
	backoff := callbackInitialBackoff
	for attempt := 1; ; attempt++ {
		sentAt := s.clock.Now()
		status, err := s.post(deliveryID, body)
		s.record(jobID, callbackAttempt{
			DeliveryID: deliveryID,
			Attempt:    attempt,
			SentAt:     sentAt,
			StatusCode: status,
			Error:      errorString(err),
		})
		args := []any{
			"job", jobID,
			"event", callbackEventCompleted,
			"delivery", deliveryID,
			"attempt", attempt,
			"status", status,
		}
		if err != nil {
			args = append(args, "error", err)
		}
		s.logger.Info("callback attempt", args...)

		if err == nil && status >= http.StatusOK && status < http.StatusMultipleChoices {
			return true
		}
		if !s.sleep(backoff) {
			return false
		}
		if backoff < callbackMaxBackoff {
			backoff *= 2
			if backoff > callbackMaxBackoff {
				backoff = callbackMaxBackoff
			}
		}
	}
}

// post makes one signed attempt and returns the response status. The signing
// key is read from disk before every attempt, as the file is shared with the
// control plane and may be rotated.
func (s *callbackSender) post(deliveryID string, body []byte) (int, error) {
	key, err := readSigningKey(s.signingKeyFile)
	if err != nil {
		return 0, fmt.Errorf("read signing key: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.callbackURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build callback request: %w", err)
	}
	req.Header.Set(callbackEventHeader, callbackEventCompleted)
	req.Header.Set(callbackDeliveryHeader, deliveryID)
	req.Header.Set(callbackSignatureHeader, signature)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// sleep waits for d on the injected clock. A non-positive duration returns at
// once. It reports false when the sender closes before the wait ends.
func (s *callbackSender) sleep(d time.Duration) bool {
	if s.ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := s.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-s.ctx.Done():
		return false
	}
}

// record appends one attempt to the job's delivery history.
func (s *callbackSender) record(jobID string, attempt callbackAttempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries[jobID] = append(s.deliveries[jobID], attempt)
}

// attempts returns a copy of the delivery history of jobID, in send order.
func (s *callbackSender) attempts(jobID string) []callbackAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]callbackAttempt(nil), s.deliveries[jobID]...)
}

// close cancels pending delays, retries and in-flight requests. Pending
// callbacks are dropped, matching the restart semantics of extsim serve. A
// send started after close is discarded.
func (s *callbackSender) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cancel()
}

// idle waits until every started delivery has returned. It is used by tests
// and by shutdown when callbacks are allowed to finish.
func (s *callbackSender) idle() {
	s.wg.Wait()
}

// readSigningKey reads the shared HMAC key and removes one trailing newline,
// either "\r\n" or "\n", never both. The remaining bytes are used verbatim.
func readSigningKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasSuffix(key, []byte("\r\n")) {
		return key[:len(key)-2], nil
	}
	if bytes.HasSuffix(key, []byte("\n")) {
		return key[:len(key)-1], nil
	}
	return key, nil
}

// newCallbackDeliveryID returns a random UUID (version 4) used as the
// X-Extsim-Delivery value of one delivery.
func newCallbackDeliveryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// errorString renders err for a callbackAttempt, leaving it empty on success.
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
