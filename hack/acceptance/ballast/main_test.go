package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestFillPagesDiffer reproduces the review finding: the original fill copied
// one repeated block into every page, so every page held identical bytes and a
// checksum over the whole ballast could not tell an intact block from one
// whose pages were swapped or shifted.
func TestFillPagesDiffer(t *testing.T) {
	b := fill(4 * pageSize)
	first := b[:pageSize]
	for page := 1; page < 4; page++ {
		got := b[page*pageSize : (page+1)*pageSize]
		if bytes.Equal(got, first) {
			t.Fatalf("page %d has the same content as page 0", page)
		}
	}
}

// TestFillIsDeterministic proves that a page's bytes depend only on its page
// number: a fresh fill regenerates exactly the same block, and every page
// matches its own page-number seed.
func TestFillIsDeterministic(t *testing.T) {
	const n = 8
	a := fill(n * pageSize)
	b := fill(n * pageSize)
	if !bytes.Equal(a, b) {
		t.Fatal("fill is not deterministic")
	}
	for page := 0; page < n; page++ {
		want := make([]byte, pageSize)
		fillPage(want, page)
		if !bytes.Equal(a[page*pageSize:(page+1)*pageSize], want) {
			t.Fatalf("page %d does not match its seeded content", page)
		}
	}
}

// TestVerifyIntact proves that an untouched block verifies.
func TestVerifyIntact(t *testing.T) {
	b := fill(8 * pageSize)
	if ok, page, offset := verify(b); !ok {
		t.Fatalf("intact block reported as corrupt at page %d offset %d", page, offset)
	}
}

// TestVerifyDetectsPageSwap covers the mutation check: swapping the content of
// two pages must fail, and the reported page must be the first swapped page.
func TestVerifyDetectsPageSwap(t *testing.T) {
	b := fill(8 * pageSize)
	swapPages(b, 2, 3)

	ok, page, offset := verify(b)
	if ok {
		t.Fatal("swapped pages were not detected")
	}
	if page != 2 {
		t.Fatalf("first mismatch page = %d, want 2", page)
	}
	if offset < int64(2*pageSize) || offset >= int64(3*pageSize) {
		t.Fatalf("first mismatch offset = %d, want inside page 2", offset)
	}
}

// TestVerifyDetectsShift covers a misaligned restore: shifting the block by one
// page must be reported at the first page, not absorbed by a whole-block
// checksum.
func TestVerifyDetectsShift(t *testing.T) {
	b := fill(8 * pageSize)
	shifted := make([]byte, len(b))
	copy(shifted[pageSize:], b[:len(b)-pageSize])
	copy(shifted[:pageSize], b[len(b)-pageSize:])

	ok, page, _ := verify(shifted)
	if ok {
		t.Fatal("shifted block was not detected")
	}
	if page != 0 {
		t.Fatalf("first mismatch page = %d, want 0", page)
	}
}

// newH2CTestServer serves handler over cleartext HTTP/2 with prior knowledge,
// the same protocol the reporter uses through the egress gateway.
func newH2CTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func waitForDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reporter did not stop after its context was cancelled")
	}
}

// TestReporterSendsReportsOverH2C reproduces the access-path criterion: the
// report leaves the actor as cleartext HTTP/2 with prior knowledge, carries
// the startup identity, the page-by-page verification and the resident set
// size, and advances the sequence only after the host accepts it.
func TestReporterSendsReportsOverH2C(t *testing.T) {
	old := ballast
	ballast = fill(8 * pageSize)
	t.Cleanup(func() { ballast = old })

	var mu sync.Mutex
	var reports []report
	var protos, paths, methods []string

	server := newH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		reports = append(reports, rep)
		protos = append(protos, r.Proto)
		paths = append(paths, r.URL.Path)
		methods = append(methods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))

	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReporter(ctx, reporterConfig{
			URL:        server.URL + reportPath,
			ActorName:  "cw-testactor",
			StartupID:  "0123456789abcdef",
			StartedAt:  startedAt,
			CRC32:      "deadbeef",
			PID:        4242,
			Interval:   20 * time.Millisecond,
			BackoffMin: 5 * time.Millisecond,
			BackoffMax: 20 * time.Millisecond,
		})
	}()

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reports) >= 3
	})
	cancel()
	waitForDone(t, done)

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 3; i++ {
		if methods[i] != http.MethodPost {
			t.Errorf("report %d method = %s, want POST", i, methods[i])
		}
		if paths[i] != reportPath {
			t.Errorf("report %d path = %s, want %s", i, paths[i], reportPath)
		}
		if protos[i] != "HTTP/2.0" {
			t.Errorf("report %d protocol = %s, want HTTP/2.0", i, protos[i])
		}
	}

	for i := 0; i < 3; i++ {
		got := reports[i]
		if got.ActorName != "cw-testactor" || got.StartupID != "0123456789abcdef" {
			t.Errorf("report %d identity = %q/%q, want cw-testactor/0123456789abcdef",
				i, got.ActorName, got.StartupID)
		}
		if got.StartedAt != startedAt || got.PID != 4242 || got.CRC32 != "deadbeef" {
			t.Errorf("report %d startup fields = %q/%d/%q, want %q/4242/deadbeef",
				i, got.StartedAt, got.PID, got.CRC32, startedAt)
		}
		if got.Sequence != int64(i+1) {
			t.Errorf("report %d sequence = %d, want %d", i, got.Sequence, i+1)
		}
		if got.BallastBytes != 8*pageSize || got.BallastMiB != ballastMiB {
			t.Errorf("report %d ballast = %d bytes/%d MiB, want %d bytes/%d MiB",
				i, got.BallastBytes, got.BallastMiB, 8*pageSize, ballastMiB)
		}
		if got.RSSError != "" || got.RSSBytes <= 0 {
			t.Errorf("report %d rss = %d (%s), want a positive resident set size", i, got.RSSBytes, got.RSSError)
		}
		if !got.Verify.Match || got.Verify.Pages != 8 || got.Verify.PageSize != pageSize {
			t.Errorf("report %d verify = %+v, want a match over 8 pages of %d bytes",
				i, got.Verify, pageSize)
		}
		if got.Verify.FirstMismatchPage != nil || got.Verify.FirstMismatchOffset != nil {
			t.Errorf("report %d has a mismatch page although the block matches: %+v", i, got.Verify)
		}
	}
}

// TestReportDetectsMismatch proves that a report carries the first page that
// differs, not just a boolean, so a restored actor that swapped or shifted
// pages can be diagnosed.
func TestReportDetectsMismatch(t *testing.T) {
	old := ballast
	ballast = fill(8 * pageSize)
	t.Cleanup(func() { ballast = old })
	ballast[3*pageSize] ^= 0xff

	rep := buildReport(reporterConfig{ActorName: "cw-testactor", PID: 1}, 1)
	if rep.Verify.Match {
		t.Fatal("corrupted block reported as matching")
	}
	if rep.Verify.FirstMismatchPage == nil || *rep.Verify.FirstMismatchPage != 3 {
		t.Fatalf("first mismatch page = %v, want 3", rep.Verify.FirstMismatchPage)
	}
	if rep.Verify.FirstMismatchOffset == nil || *rep.Verify.FirstMismatchOffset != 3*pageSize {
		t.Fatalf("first mismatch offset = %v, want %d", rep.Verify.FirstMismatchOffset, 3*pageSize)
	}
}

func swapPages(b []byte, i, j int) {
	a := b[i*pageSize : (i+1)*pageSize]
	c := b[j*pageSize : (j+1)*pageSize]
	tmp := make([]byte, pageSize)
	copy(tmp, a)
	copy(a, c)
	copy(c, tmp)
}
