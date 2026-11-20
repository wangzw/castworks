package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
)

// fakeCallbackClock adapts fakeclock to the shared Clock interface.
// fakeclock.NewTimer returns a runcontroller.Timer; the concrete
// *fakeclock.Timer satisfies Timer, so the wrapper only changes the static
// return type.
type fakeCallbackClock struct {
	*fakeclock.Clock
}

func (c fakeCallbackClock) NewTimer(d time.Duration) Timer {
	return c.Clock.NewTimer(d)
}

var callbackTestStart = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func newFakeCallbackClock() fakeCallbackClock {
	return fakeCallbackClock{Clock: fakeclock.New(callbackTestStart)}
}

func discardCallbackLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeSigningKey(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing signing key: %v", err)
	}
	return path
}

func signCallbackBody(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// waitForCallbackTimer blocks until the sender has one pending timer, which
// happens after a failed attempt or during the first-send delay.
func waitForCallbackTimer(t *testing.T, clock fakeCallbackClock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clock.WaitForTimers(ctx, 1); err != nil {
		t.Fatalf("waiting for callback timer: %v", err)
	}
}

// callbackRequest is one request as the receiver saw it.
type callbackRequest struct {
	method    string
	event     string
	delivery  string
	signature string
	body      []byte
}

func newCallbackRecorder(status func(n int32, req callbackRequest) int) (*httptest.Server, chan callbackRequest, *atomic.Int32) {
	requests := make(chan callbackRequest, 16)
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		req := callbackRequest{
			method:    r.Method,
			event:     r.Header.Get(callbackEventHeader),
			delivery:  r.Header.Get(callbackDeliveryHeader),
			signature: r.Header.Get(callbackSignatureHeader),
			body:      body,
		}
		requests <- req
		w.WriteHeader(status(n, req))
	}))
	return srv, requests, &count
}

func TestCallbackNewPlan(t *testing.T) {
	tests := []struct {
		name     string
		callback string
		delayMS  int
		want     callbackKind
		wantErr  bool
	}{
		{name: "empty means deliver", want: callbackDeliver},
		{name: "deliver", callback: "deliver", want: callbackDeliver},
		{name: "drop with delay", callback: "drop", delayMS: 250, want: callbackDrop},
		{name: "duplicate", callback: "duplicate", want: callbackDuplicate},
		{name: "unknown callback", callback: "retry", wantErr: true},
		{name: "negative delay", callback: "deliver", delayMS: -1, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := newCallbackPlan(tc.callback, tc.delayMS)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("newCallbackPlan(%q, %d) succeeded, want error", tc.callback, tc.delayMS)
				}
				return
			}
			if err != nil {
				t.Fatalf("newCallbackPlan(%q, %d): %v", tc.callback, tc.delayMS, err)
			}
			if plan.Callback != tc.want {
				t.Errorf("callback = %q, want %q", plan.Callback, tc.want)
			}
			if plan.CallbackDelayMS != tc.delayMS {
				t.Errorf("callback_delay_ms = %d, want %d", plan.CallbackDelayMS, tc.delayMS)
			}
		})
	}
}

func TestCallbackDeliver(t *testing.T) {
	keyFile := writeSigningKey(t, "secret-key")
	srv, requests, _ := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
	defer srv.Close()

	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyFile, clock, discardCallbackLogger())
	defer sender.close()

	body := []byte(`{"job":{"id":"job-abcdefghijkl","status":"completed"}}`)
	plan, err := newCallbackPlan("", 0)
	if err != nil {
		t.Fatalf("newCallbackPlan: %v", err)
	}
	sender.send("job-abcdefghijkl", body, plan)
	sender.idle()

	var got callbackRequest
	select {
	case got = <-requests:
	default:
		t.Fatal("no callback request was received")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.event != callbackEventCompleted {
		t.Errorf("event header = %q, want %q", got.event, callbackEventCompleted)
	}
	if got.signature != signCallbackBody("secret-key", body) {
		t.Errorf("signature = %q, want HMAC-SHA256 of the body with the key", got.signature)
	}
	if string(got.body) != string(body) {
		t.Errorf("body = %q, want %q", got.body, body)
	}
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidPattern.MatchString(got.delivery) {
		t.Errorf("delivery id = %q, want a version 4 UUID", got.delivery)
	}

	attempts := sender.attempts("job-abcdefghijkl")
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1: %+v", len(attempts), attempts)
	}
	if attempts[0].DeliveryID != got.delivery {
		t.Errorf("recorded delivery id = %q, want header value %q", attempts[0].DeliveryID, got.delivery)
	}
	if attempts[0].Attempt != 1 {
		t.Errorf("attempt number = %d, want 1", attempts[0].Attempt)
	}
	if attempts[0].StatusCode != http.StatusOK {
		t.Errorf("status code = %d, want 200", attempts[0].StatusCode)
	}
	if attempts[0].Error != "" {
		t.Errorf("error = %q, want empty", attempts[0].Error)
	}
	if !attempts[0].SentAt.Equal(callbackTestStart) {
		t.Errorf("sent at = %v, want %v", attempts[0].SentAt, callbackTestStart)
	}
}

func TestCallbackSignatureKeyNewlines(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     string
	}{
		{name: "no newline", contents: "k3y", want: "k3y"},
		{name: "lf", contents: "k3y\n", want: "k3y"},
		{name: "crlf", contents: "k3y\r\n", want: "k3y"},
		{name: "lf before crlf", contents: "k3y\n\r\n", want: "k3y\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyFile := writeSigningKey(t, tc.contents)
			srv, requests, _ := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
			defer srv.Close()

			sender := newCallbackSender(srv.URL, keyFile, newFakeCallbackClock(), discardCallbackLogger())
			defer sender.close()

			body := []byte(`{"job":{"id":"job-1"}}`)
			sender.send("job-1", body, callbackPlan{Callback: callbackDeliver})
			sender.idle()

			select {
			case got := <-requests:
				if want := signCallbackBody(tc.want, body); got.signature != want {
					t.Errorf("signature = %q, want %q (the trailing newline must not be part of the key)", got.signature, want)
				}
			default:
				t.Fatal("no callback request was received")
			}
		})
	}
}

func TestCallbackRereadsSigningKeyEachAttempt(t *testing.T) {
	body := []byte(`{"job":{"id":"job-reread"}}`)
	oldSignature := signCallbackBody("old-key", body)

	srv, requests, _ := newCallbackRecorder(func(_ int32, req callbackRequest) int {
		if req.signature == oldSignature {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	defer srv.Close()

	keyPath := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(keyPath, []byte("old-key\n"), 0o600); err != nil {
		t.Fatalf("writing old key: %v", err)
	}

	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyPath, clock, discardCallbackLogger())
	defer sender.close()

	sender.send("job-reread", body, callbackPlan{Callback: callbackDeliver})
	waitForCallbackTimer(t, clock)

	if err := os.WriteFile(keyPath, []byte("new-key\r\n"), 0o600); err != nil {
		t.Fatalf("writing new key: %v", err)
	}
	clock.Advance(time.Second)
	sender.idle()

	var got []callbackRequest
	for len(requests) > 0 {
		got = append(got, <-requests)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].signature != oldSignature {
		t.Errorf("first signature = %q, want the old key's signature", got[0].signature)
	}
	if want := signCallbackBody("new-key", body); got[1].signature != want {
		t.Errorf("second signature = %q, want %q (the key is read before every attempt)", got[1].signature, want)
	}
}

func TestCallbackRetriesUntilSuccess(t *testing.T) {
	srv, _, count := newCallbackRecorder(func(n int32, _ callbackRequest) int {
		if n < 3 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyFile, clock, discardCallbackLogger())
	defer sender.close()

	body := []byte(`{"job":{"id":"job-retry"}}`)
	sender.send("job-retry", body, callbackPlan{Callback: callbackDeliver})

	// First failure: the retry timer is one second out.
	waitForCallbackTimer(t, clock)
	clock.Advance(time.Second)
	// Second failure: the next interval doubles to two seconds.
	waitForCallbackTimer(t, clock)
	clock.Advance(2 * time.Second)
	sender.idle()

	if got := count.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (two failures then a success)", got)
	}
	attempts := sender.attempts("job-retry")
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3: %+v", len(attempts), attempts)
	}
	for i, wantStatus := range []int{500, 500, 200} {
		if attempts[i].Attempt != i+1 {
			t.Errorf("attempt[%d].Attempt = %d, want %d", i, attempts[i].Attempt, i+1)
		}
		if attempts[i].StatusCode != wantStatus {
			t.Errorf("attempt[%d].StatusCode = %d, want %d", i, attempts[i].StatusCode, wantStatus)
		}
		if attempts[i].DeliveryID != attempts[0].DeliveryID {
			t.Errorf("attempt[%d].DeliveryID = %q, want the same delivery id as the first attempt %q",
				i, attempts[i].DeliveryID, attempts[0].DeliveryID)
		}
	}
	wantTimes := []time.Time{
		callbackTestStart,
		callbackTestStart.Add(time.Second),
		callbackTestStart.Add(3 * time.Second),
	}
	for i, want := range wantTimes {
		if !attempts[i].SentAt.Equal(want) {
			t.Errorf("attempt[%d].SentAt = %v, want %v", i, attempts[i].SentAt, want)
		}
	}
}

func TestCallbackRetriesOnConnectionFailure(t *testing.T) {
	// Reserve an address, then close it so every attempt is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving address: %v", err)
	}
	url := "http://" + ln.Addr().String() + "/hooks/extsim"
	if err := ln.Close(); err != nil {
		t.Fatalf("closing reserved address: %v", err)
	}

	keyFile := writeSigningKey(t, "secret-key")
	clock := newFakeCallbackClock()
	sender := newCallbackSender(url, keyFile, clock, discardCallbackLogger())

	body := []byte(`{"job":{"id":"job-refused"}}`)
	sender.send("job-refused", body, callbackPlan{Callback: callbackDeliver})

	waitForCallbackTimer(t, clock)
	first := sender.attempts("job-refused")
	if len(first) != 1 {
		t.Fatalf("attempts after the first failure = %d, want 1", len(first))
	}
	if first[0].StatusCode != 0 || first[0].Error == "" {
		t.Errorf("first attempt = %+v, want status 0 and a connection error", first[0])
	}

	clock.Advance(time.Second)
	// A second attempt has completed and scheduled the two-second retry.
	waitForCallbackTimer(t, clock)
	second := sender.attempts("job-refused")
	if len(second) != 2 {
		t.Fatalf("attempts after the retry = %d, want 2", len(second))
	}
	if second[1].DeliveryID != second[0].DeliveryID {
		t.Errorf("retry delivery id = %q, want the first delivery id %q", second[1].DeliveryID, second[0].DeliveryID)
	}

	sender.close()
	sender.idle()
}

func TestCallbackDropSendsNothing(t *testing.T) {
	srv, _, count := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	sender := newCallbackSender(srv.URL, keyFile, newFakeCallbackClock(), discardCallbackLogger())
	defer sender.close()

	sender.send("job-dropped", []byte(`{"job":{"id":"job-dropped"}}`), callbackPlan{Callback: callbackDrop})
	sender.idle()

	if got := count.Load(); got != 0 {
		t.Errorf("requests = %d, want 0 for a dropped callback", got)
	}
	if attempts := sender.attempts("job-dropped"); len(attempts) != 0 {
		t.Errorf("attempts = %+v, want none for a dropped callback", attempts)
	}
}

func TestCallbackDuplicate(t *testing.T) {
	srv, requests, _ := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	sender := newCallbackSender(srv.URL, keyFile, newFakeCallbackClock(), discardCallbackLogger())
	defer sender.close()

	body := []byte(`{"job":{"id":"job-duplicate"}}`)
	sender.send("job-duplicate", body, callbackPlan{Callback: callbackDuplicate})
	sender.idle()

	var got []callbackRequest
	for len(requests) > 0 {
		got = append(got, <-requests)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2 for a duplicate plan", len(got))
	}
	if string(got[0].body) != string(got[1].body) {
		t.Errorf("second body = %q, want the same body as the first %q", got[1].body, got[0].body)
	}
	if got[0].signature != got[1].signature {
		t.Errorf("second signature = %q, want the same signature as the first %q", got[1].signature, got[0].signature)
	}
	if got[0].delivery == got[1].delivery {
		t.Errorf("both deliveries used delivery id %q, want a new id for the second", got[0].delivery)
	}

	attempts := sender.attempts("job-duplicate")
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	for i, attempt := range attempts {
		if attempt.Attempt != 1 {
			t.Errorf("attempts[%d].Attempt = %d, want 1 (the counter restarts per delivery)", i, attempt.Attempt)
		}
		if attempt.StatusCode != http.StatusOK {
			t.Errorf("attempts[%d].StatusCode = %d, want 200", i, attempt.StatusCode)
		}
	}
	if attempts[0].DeliveryID != got[0].delivery || attempts[1].DeliveryID != got[1].delivery {
		t.Errorf("recorded delivery ids %q, %q do not match the header values %q, %q",
			attempts[0].DeliveryID, attempts[1].DeliveryID, got[0].delivery, got[1].delivery)
	}
}

// TestCallbackDuplicateWaitsForFirstDeliverySuccess checks that a duplicate
// plan does not send its second delivery until the first delivery has received
// a 2xx, by making the first attempt fail and observing when the duplicate is
// sent.
func TestCallbackDuplicateWaitsForFirstDeliverySuccess(t *testing.T) {
	srv, requests, _ := newCallbackRecorder(func(n int32, _ callbackRequest) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyFile, clock, discardCallbackLogger())
	defer sender.close()

	const jobID = "job-duplicate-retry"
	body := []byte(`{"job":{"id":"job-duplicate-retry"}}`)
	sender.send(jobID, body, callbackPlan{Callback: callbackDuplicate})

	// The first attempt failed and scheduled a retry. The duplicate must not
	// have been sent: no 2xx has arrived yet.
	waitForCallbackTimer(t, clock)
	if got := len(requests); got != 1 {
		t.Fatalf("requests before the first delivery succeeds = %d, want 1; the duplicate must wait for a 2xx", got)
	}
	if got := len(sender.attempts(jobID)); got != 1 {
		t.Fatalf("recorded attempts before the retry = %d, want 1", got)
	}

	// The retry succeeds; only now is the duplicate sent.
	clock.Advance(time.Second)
	sender.idle()

	var got []callbackRequest
	for len(requests) > 0 {
		got = append(got, <-requests)
	}
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3 (failed attempt, successful retry, duplicate): %+v", len(got), got)
	}
	if got[0].delivery != got[1].delivery {
		t.Errorf("retry delivery id = %q, want the first delivery id %q", got[1].delivery, got[0].delivery)
	}
	if got[1].delivery == got[2].delivery {
		t.Errorf("duplicate delivery id = %q, want a new id, not the first delivery's %q", got[2].delivery, got[1].delivery)
	}
	if string(got[2].body) != string(got[0].body) {
		t.Errorf("duplicate body = %q, want the same body as the first delivery %q", got[2].body, got[0].body)
	}

	attempts := sender.attempts(jobID)
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3: %+v", len(attempts), attempts)
	}
	wantStatuses := []int{http.StatusInternalServerError, http.StatusOK, http.StatusOK}
	wantAttempts := []int{1, 2, 1}
	wantTimes := []time.Time{
		callbackTestStart,
		callbackTestStart.Add(time.Second),
		callbackTestStart.Add(time.Second),
	}
	for i := range attempts {
		if attempts[i].StatusCode != wantStatuses[i] {
			t.Errorf("attempts[%d].StatusCode = %d, want %d", i, attempts[i].StatusCode, wantStatuses[i])
		}
		if attempts[i].Attempt != wantAttempts[i] {
			t.Errorf("attempts[%d].Attempt = %d, want %d", i, attempts[i].Attempt, wantAttempts[i])
		}
		if !attempts[i].SentAt.Equal(wantTimes[i]) {
			t.Errorf("attempts[%d].SentAt = %v, want %v", i, attempts[i].SentAt, wantTimes[i])
		}
	}
	if attempts[0].DeliveryID != attempts[1].DeliveryID {
		t.Errorf("attempts[1].DeliveryID = %q, want the retry to reuse %q", attempts[1].DeliveryID, attempts[0].DeliveryID)
	}
	if attempts[1].DeliveryID == attempts[2].DeliveryID {
		t.Errorf("attempts[2].DeliveryID = %q, want a new delivery id for the duplicate", attempts[2].DeliveryID)
	}
}

// TestCallbackRetryBackoffCapsAtThirtySeconds checks the retry interval
// sequence on a fake clock: one second, doubling, then thirty seconds forever.
// Each wait is split into a step one millisecond short of the expected
// interval, where no request may appear, and the final millisecond, where
// exactly one must. A backoff shorter than expected fires during the first
// step, so a constant or early-capped interval fails the test rather than
// measuring whatever the test itself advanced.
func TestCallbackRetryBackoffCapsAtThirtySeconds(t *testing.T) {
	srv, _, count := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusInternalServerError })
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyFile, clock, discardCallbackLogger())
	defer sender.close()

	const jobID = "job-backoff"
	sender.send(jobID, []byte(`{"job":{"id":"job-backoff"}}`), callbackPlan{Callback: callbackDeliver})

	// Each element is the wait before the next attempt: 1, 2, 4, 8, 16, then
	// the cap at 30 seconds, which stays.
	intervals := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, interval := range intervals {
		waitForCallbackTimer(t, clock)
		clock.Advance(interval - time.Millisecond)
		if got := callbackCountAfterSettle(count, int32(i+1)); got != int32(i+1) {
			t.Fatalf("requests after %v = %d, want %d: attempt %d fired before the %v backoff elapsed",
				interval-time.Millisecond, got, i+1, i+2, interval)
		}
		clock.Advance(time.Millisecond)
		waitForCallbackCount(t, count, int32(i+2))
	}
	// The last advance fired the last attempt; wait for the retry it scheduled
	// so the attempt has been recorded before the sender closes.
	waitForCallbackTimer(t, clock)
	sender.close()
	sender.idle()

	wantRequests := len(intervals) + 1
	if got := count.Load(); got != int32(wantRequests) {
		t.Fatalf("requests = %d, want %d", got, wantRequests)
	}
	attempts := sender.attempts(jobID)
	if len(attempts) != wantRequests {
		t.Fatalf("attempts = %d, want %d: %+v", len(attempts), wantRequests, attempts)
	}
	for i, want := range intervals {
		got := attempts[i+1].SentAt.Sub(attempts[i].SentAt)
		if got != want {
			t.Errorf("interval before attempt %d = %v, want %v", i+2, got, want)
		}
	}
}

// callbackCountAfterSettle returns the request count after giving the sender a
// short real-time window to react to the last clock advance. The fake clock
// itself does not move, so a correct backoff cannot post during the window; a
// shorter one does.
func callbackCountAfterSettle(count *atomic.Int32, want int32) int32 {
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		got := count.Load()
		if got != want || !time.Now().Before(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForCallbackCount blocks until the receiver has seen want requests.
func waitForCallbackCount(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for count.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("requests = %d, want %d after advancing the fake clock", count.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCallbackDelayPostponesFirstSend(t *testing.T) {
	srv, requests, count := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	clock := newFakeCallbackClock()
	sender := newCallbackSender(srv.URL, keyFile, clock, discardCallbackLogger())
	defer sender.close()

	body := []byte(`{"job":{"id":"job-delayed"}}`)
	sender.send("job-delayed", body, callbackPlan{Callback: callbackDeliver, CallbackDelayMS: 500})

	waitForCallbackTimer(t, clock)
	select {
	case <-requests:
		t.Fatal("callback was sent before the configured delay elapsed")
	default:
	}

	clock.Advance(500 * time.Millisecond)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("callback was not sent after the configured delay")
	}
	sender.idle()

	if got := count.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	attempts := sender.attempts("job-delayed")
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if want := callbackTestStart.Add(500 * time.Millisecond); !attempts[0].SentAt.Equal(want) {
		t.Errorf("sent at = %v, want %v", attempts[0].SentAt, want)
	}
}

// TestCallbackConcurrentJobs protects the attempt store under the race
// detector: several jobs deliver in parallel and each history stays isolated.
func TestCallbackConcurrentJobs(t *testing.T) {
	srv, _, _ := newCallbackRecorder(func(int32, callbackRequest) int { return http.StatusOK })
	defer srv.Close()

	keyFile := writeSigningKey(t, "secret-key")
	sender := newCallbackSender(srv.URL, keyFile, newFakeCallbackClock(), discardCallbackLogger())
	defer sender.close()

	ids := []string{"job-a", "job-b", "job-c", "job-d"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sender.send(id, []byte(`{"job":{"id":"`+id+`"}}`), callbackPlan{Callback: callbackDeliver})
		}(id)
	}
	wg.Wait()
	sender.idle()

	for _, id := range ids {
		attempts := sender.attempts(id)
		if len(attempts) != 1 {
			t.Errorf("%s: attempts = %d, want 1", id, len(attempts))
		}
	}
}
