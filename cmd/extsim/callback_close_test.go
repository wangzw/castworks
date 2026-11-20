package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newCloseTestReceiver returns a receiver that counts requests.
func newCloseTestReceiver(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// TestCallbackSendAfterCloseIsDropped checks that a send started after close
// is discarded instead of registering on the WaitGroup.
func TestCallbackSendAfterCloseIsDropped(t *testing.T) {
	srv, count := newCloseTestReceiver(t)
	sender := newCallbackSender(srv.URL, writeTestKey(t, "key\n"), newAssemblyTestClock(), discardTestLogger())
	sender.close()

	sender.send("job-closed", []byte(`{"job":{"id":"job-closed"}}`), callbackPlan{Callback: callbackDeliver})
	sender.idle()

	if got := count.Load(); got != 0 {
		t.Errorf("requests = %d after close, want 0", got)
	}
	if attempts := sender.attempts("job-closed"); len(attempts) != 0 {
		t.Errorf("attempts = %+v after close, want none", attempts)
	}
}

// TestCallbackSendAfterCloseSkipsDelivery checks the closed flag directly. The
// cancel function is replaced with a no-op before close, so the context stays
// usable and only the closed state can drop the send. A send after close must
// start no delivery, must not panic, and idle must return.
func TestCallbackSendAfterCloseSkipsDelivery(t *testing.T) {
	srv, count := newCloseTestReceiver(t)
	sender := newCallbackSender(srv.URL, writeTestKey(t, "key\n"), newAssemblyTestClock(), discardTestLogger())
	sender.cancel = func() {}
	sender.close()

	sender.send("job-closed", []byte(`{"job":{"id":"job-closed"}}`), callbackPlan{Callback: callbackDeliver})

	done := make(chan struct{})
	go func() {
		sender.idle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not return after a send following close")
	}

	if got := count.Load(); got != 0 {
		t.Errorf("requests = %d after close, want 0", got)
	}
	if attempts := sender.attempts("job-closed"); len(attempts) != 0 {
		t.Errorf("attempts = %+v after close, want none", attempts)
	}
}

// TestCallbackSendAfterCloseDoesNotJoinIdle repeatedly races idle with a send
// that follows close. Before the fix, send still called WaitGroup.Add while
// idle was waiting, which the runtime reports as "WaitGroup is reused before
// previous Wait has returned". After the fix, send sees closed and returns.
func TestCallbackSendAfterCloseDoesNotJoinIdle(t *testing.T) {
	srv, _ := newCloseTestReceiver(t)
	sender := newCallbackSender(srv.URL, writeTestKey(t, "key\n"), newAssemblyTestClock(), discardTestLogger())
	sender.close()

	for i := 0; i < 5000; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			sender.idle()
		}()
		go func() {
			defer wg.Done()
			sender.send("job-closed", []byte(`{"job":{"id":"job-closed"}}`), callbackPlan{Callback: callbackDeliver})
		}()
		wg.Wait()
	}
}

// TestCallbackConcurrentSendAndClose runs sends concurrently with close under
// the race detector. It must not panic or misuse the WaitGroup, and idle must
// return once the accepted deliveries finish.
func TestCallbackConcurrentSendAndClose(t *testing.T) {
	srv, _ := newCloseTestReceiver(t)
	sender := newCallbackSender(srv.URL, writeTestKey(t, "key\n"), newAssemblyTestClock(), discardTestLogger())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sender.send(fmt.Sprintf("job-%d", i), []byte(`{"job":{"id":"x"}}`), callbackPlan{Callback: callbackDeliver})
		}(i)
	}
	sender.close()
	wg.Wait()
	sender.idle()
}
