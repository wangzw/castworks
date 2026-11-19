package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
)

// streamTestDeliver builds a valid Deliver frame whose delivery_id identifies
// it in ordering assertions.
func streamTestDeliver(deliveryID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: deliveryID,
			Batch:      []*agentprotocolv1.InboxMessage{{InboxSeq: 1, Text: "hello"}},
			ThroughSeq: 1,
		}},
	}
}

func streamTestCollector(sent *[]string) func(*agentprotocolv1.ControlFrame) error {
	return func(f *agentprotocolv1.ControlFrame) error {
		*sent = append(*sent, f.GetDeliver().GetDeliveryId())
		return nil
	}
}

func TestStreamRegistryReplaceReturnsPreviousStream(t *testing.T) {
	reg := newStreamRegistry()
	first := newConn("run-1")
	second := newConn("run-1")
	other := newConn("run-2")

	if old := reg.replace("run-1", first); old != nil {
		t.Fatalf("replace on an empty registry returned %v, want nil", old)
	}
	if got := reg.current("run-1"); got != first {
		t.Fatalf("current = %v, want the first conn", got)
	}

	if old := reg.replace("run-1", second); old != first {
		t.Fatalf("replace returned %v, want the first conn it replaced", old)
	}
	if got := reg.current("run-1"); got != second {
		t.Fatalf("current = %v, want the second conn", got)
	}
	if got := reg.current("run-2"); got != nil {
		t.Fatalf("current(run-2) = %v, want nil", got)
	}
	if got := reg.replace("run-2", other); got != nil {
		t.Fatalf("replace(run-2) returned %v, want nil", got)
	}
}

func TestStreamRegistryRemoveIfOnlyRemovesTheCurrentStream(t *testing.T) {
	reg := newStreamRegistry()
	first := newConn("run-1")
	second := newConn("run-1")

	reg.replace("run-1", first)
	if !reg.removeIf("run-1", first) {
		t.Fatal("removeIf(current) = false, want true")
	}
	if got := reg.current("run-1"); got != nil {
		t.Fatalf("current after removeIf = %v, want nil", got)
	}

	reg.replace("run-1", first)
	reg.replace("run-1", second)
	if reg.removeIf("run-1", first) {
		t.Fatal("removeIf(superseded conn) = true, want false")
	}
	if got := reg.current("run-1"); got != second {
		t.Fatalf("superseded removeIf changed the current stream to %v", got)
	}

	if reg.removeIf("run-2", first) {
		t.Fatal("removeIf on an unknown run = true, want false")
	}
	if reg.removeIf("run-1", nil) {
		t.Fatal("removeIf(nil) = true, want false")
	}
	if got := reg.current("run-1"); got != second {
		t.Fatalf("removeIf(nil) changed the current stream to %v", got)
	}
}

func TestStreamConnEnqueueRejectsInvalidFrames(t *testing.T) {
	invalid := map[string]*agentprotocolv1.ControlFrame{
		"nil frame": nil,
		"no run id": {
			Body: &agentprotocolv1.ControlFrame_Shutdown{Shutdown: &agentprotocolv1.Shutdown{}},
		},
		"no body": {RunId: "run-1"},
		"deliver without delivery id": {
			RunId: "run-1",
			Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
				Batch:      []*agentprotocolv1.InboxMessage{{InboxSeq: 1, Text: "x"}},
				ThroughSeq: 1,
			}},
		},
	}

	c := newConn("run-1")
	for name, frame := range invalid {
		if err := c.enqueue(frame); err == nil {
			t.Errorf("enqueue(%s) = nil, want a validation error", name)
		}
	}

	if err := c.enqueue(streamTestDeliver("d-1")); err != nil {
		t.Fatalf("enqueue(valid frame) = %v, want nil", err)
	}

	c.closeWith(status.New(codes.OK, ""))
	var sent []string
	if err := c.pump(context.Background(), streamTestCollector(&sent)); err != nil {
		t.Fatalf("pump = %v, want nil", err)
	}
	if len(sent) != 1 || sent[0] != "d-1" {
		t.Fatalf("sent = %v, want only the valid frame [d-1]", sent)
	}
}

func TestStreamConnEnqueueAfterCloseFails(t *testing.T) {
	c := newConn("run-1")
	c.closeWith(status.New(codes.Aborted, "replaced"))

	if err := c.enqueue(streamTestDeliver("d-1")); !errors.Is(err, errStreamClosed) {
		t.Fatalf("enqueue after close = %v, want errStreamClosed", err)
	}

	var sent []string
	if err := c.pump(context.Background(), streamTestCollector(&sent)); status.Code(err) != codes.Aborted {
		t.Fatalf("pump = %v, want Aborted", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent = %v, want no frames", sent)
	}
}

func TestStreamConnCloseWithDrainsQueuedFramesFirst(t *testing.T) {
	c := newConn("run-1")
	for _, id := range []string{"d-1", "d-2", "d-3"} {
		if err := c.enqueue(streamTestDeliver(id)); err != nil {
			t.Fatalf("enqueue(%s) = %v", id, err)
		}
	}
	c.closeWith(status.New(codes.Aborted, "replaced by a new stream"))

	var sent []string
	err := c.pump(context.Background(), streamTestCollector(&sent))
	if status.Code(err) != codes.Aborted {
		t.Fatalf("pump = %v, want Aborted", err)
	}
	want := []string{"d-1", "d-2", "d-3"}
	if fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}

	select {
	case <-c.done():
	default:
		t.Fatal("done() is not closed after closeWith")
	}
}

func TestStreamConnCloseWithIsIdempotent(t *testing.T) {
	c := newConn("run-1")
	c.closeWith(status.New(codes.Aborted, "first"))
	c.closeWith(status.New(codes.Internal, "second"))

	var sent []string
	err := c.pump(context.Background(), streamTestCollector(&sent))
	if status.Code(err) != codes.Aborted {
		t.Fatalf("pump = %v, want the first close status Aborted", err)
	}
	if got := status.Convert(err).Message(); got != "first" {
		t.Fatalf("close message = %q, want the first one %q", got, "first")
	}
}

func TestStreamConnCloseWithOKReturnsNil(t *testing.T) {
	c := newConn("run-1")
	if err := c.enqueue(streamTestDeliver("d-1")); err != nil {
		t.Fatalf("enqueue = %v", err)
	}
	c.closeWith(status.New(codes.OK, ""))

	var sent []string
	if err := c.pump(context.Background(), streamTestCollector(&sent)); err != nil {
		t.Fatalf("pump = %v, want nil for codes.OK", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %v, want the queued frame", sent)
	}
}

func TestStreamConnPumpStopsOnSendFailure(t *testing.T) {
	c := newConn("run-1")
	for _, id := range []string{"d-1", "d-2", "d-3"} {
		if err := c.enqueue(streamTestDeliver(id)); err != nil {
			t.Fatalf("enqueue(%s) = %v", id, err)
		}
	}
	c.closeWith(status.New(codes.OK, ""))

	sendErr := errors.New("transport gone")
	calls := 0
	err := c.pump(context.Background(), func(*agentprotocolv1.ControlFrame) error {
		calls++
		if calls == 2 {
			return sendErr
		}
		return nil
	})
	if !errors.Is(err, sendErr) {
		t.Fatalf("pump = %v, want the send error", err)
	}
	if calls != 2 {
		t.Fatalf("send called %d times, want 2 (the rest abandoned)", calls)
	}
}

func TestStreamConnPumpWakesWhenClosedWhileWaiting(t *testing.T) {
	c := newConn("run-1")
	pending, err := c.reserve(streamTestDeliver("d-1"), 1)
	if err != nil {
		t.Fatalf("reserve = %v", err)
	}

	var sent []string
	pumped := make(chan error, 1)
	go func() {
		pumped <- c.pump(context.Background(), streamTestCollector(&sent))
	}()

	// Wait until pump has dequeued the slot and is blocked on its decision.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		dequeued := len(c.queue) == 0
		c.mu.Unlock()
		if dequeued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pump 没有取出待放行的位置")
		}
		time.Sleep(time.Millisecond)
	}

	c.closeWith(status.New(codes.Aborted, "closed while waiting"))
	select {
	case perr := <-pumped:
		if status.Code(perr) != codes.Aborted {
			t.Fatalf("pump = %v, 想要 Aborted", perr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("流关闭没有唤醒等待未放行位置的 pump")
	}

	// The unreleased position was dropped; releasing it afterwards must not
	// make it leave on a closed stream.
	pending.release()
	if len(sent) != 0 {
		t.Fatalf("关闭的流发出了未放行的帧: %v", sent)
	}
}

func TestStreamConnPumpStopsOnContextDone(t *testing.T) {
	c := newConn("run-1")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.pump(ctx, func(*agentprotocolv1.ControlFrame) error { return nil })
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pump = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not return after ctx was canceled")
	}
}

func TestStreamConnConcurrentEnqueueKeepsPerGoroutineOrder(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 200

	c := newConn("run-1")
	var sent []string
	pumped := make(chan error, 1)
	go func() {
		pumped <- c.pump(context.Background(), streamTestCollector(&sent))
	}()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := c.enqueue(streamTestDeliver(fmt.Sprintf("g%d-%03d", g, i))); err != nil {
					t.Errorf("enqueue(g%d-%03d) = %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	c.closeWith(status.New(codes.OK, ""))

	if err := <-pumped; err != nil {
		t.Fatalf("pump = %v", err)
	}
	if len(sent) != goroutines*perGoroutine {
		t.Fatalf("sent %d frames, want %d", len(sent), goroutines*perGoroutine)
	}

	// Each goroutine's relative order must survive the interleaving; the
	// overall order is not fixed because goroutines race.
	last := make(map[int]int, goroutines)
	for _, id := range sent {
		var g, seq int
		if _, err := fmt.Sscanf(id, "g%d-%d", &g, &seq); err != nil {
			t.Fatalf("unexpected frame id %q: %v", id, err)
		}
		if prev, ok := last[g]; ok && seq <= prev {
			t.Fatalf("frame %s arrived after %s from the same goroutine", id, fmt.Sprintf("g%d-%03d", g, prev))
		}
		last[g] = seq
	}
	if len(last) != goroutines {
		t.Fatalf("saw frames from %d goroutines, want %d", len(last), goroutines)
	}
}

func TestStreamConnConcurrentOperations(t *testing.T) {
	const workers = 16
	const iterations = 300

	c := newConn("run-1")
	pumped := make(chan error, 1)
	go func() {
		pumped <- c.pump(context.Background(), func(*agentprotocolv1.ControlFrame) error { return nil })
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Enqueue errors after close are expected; only the frame
				// validation path must never fail.
				_ = c.enqueue(streamTestDeliver(fmt.Sprintf("g%d-%d", w, i)))
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = c.id()
		}
		c.closeWith(status.New(codes.Aborted, "replaced"))
		c.closeWith(status.New(codes.Internal, "ignored"))
	}()
	wg.Wait()

	if err := <-pumped; status.Code(err) != codes.Aborted {
		t.Fatalf("pump = %v, want Aborted", err)
	}
	if !errors.Is(c.enqueue(streamTestDeliver("late")), errStreamClosed) {
		t.Fatal("enqueue after the concurrent close did not report errStreamClosed")
	}
}

func TestStreamConnIDsAreUnique(t *testing.T) {
	const n = 500
	ids := make(chan uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- newConn("run-1").id()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[uint64]bool, n)
	for id := range ids {
		if seen[id] {
			t.Fatalf("conn id %d was handed out twice", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique ids, want %d", len(seen), n)
	}
}

func TestStreamConnRunID(t *testing.T) {
	if got := newConn("run-7").runID(); got != "run-7" {
		t.Fatalf("runID() = %q, want run-7", got)
	}
}

func TestStreamRegistryConcurrentOperations(t *testing.T) {
	const workers = 16
	const iterations = 300

	reg := newStreamRegistry()
	var wg sync.WaitGroup

	// One goroutine keeps replacing the current stream and closing the old
	// one, mirroring what a new Hello does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			c := newConn("run-1")
			if old := reg.replace("run-1", c); old != nil {
				old.closeWith(status.New(codes.Aborted, "superseded"))
			}
			// A racing worker may already have closed this conn; only the
			// absence of a data race matters here.
			_ = c.enqueue(streamTestDeliver(fmt.Sprintf("r-%d", i)))
		}
	}()

	// The rest race current, removeIf, enqueue and closeWith against it.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				c := reg.current("run-1")
				if c == nil {
					continue
				}
				_ = c.enqueue(streamTestDeliver(fmt.Sprintf("w%d-%d", w, i)))
				if i%7 == 0 {
					c.closeWith(status.New(codes.OK, ""))
				}
				reg.removeIf("run-1", c)
			}
		}(w)
	}
	wg.Wait()
}
