package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// newH2CTestServer serves handler over cleartext HTTP/2 with prior knowledge,
// the same protocol the reporter uses to reach the control plane.
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

func TestReporterSendsReportsOverH2C(t *testing.T) {
	var mu sync.Mutex
	var reports []Report
	var protos, paths, methods []string

	server := newH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
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

	statePath := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(statePath, []byte("fedcba9876543210"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReporter(ctx, reporterConfig{
			URL:        server.URL + reportPath,
			ActorName:  "cw-testactor",
			StartupID:  "0123456789abcdef",
			StatePath:  statePath,
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

	want := Report{
		ActorName:   "cw-testactor",
		StartupID:   "0123456789abcdef",
		FileContent: "fedcba9876543210",
		PID:         4242,
	}
	for i := 0; i < 3; i++ {
		got := reports[i]
		want.Counter = int64(i + 1)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("report %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestReporterRetriesFailedReports(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	success := make(chan Report, 8)

	server := newH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n <= 2 {
			http.Error(w, "not yet", http.StatusInternalServerError)
			return
		}
		success <- rep
		w.WriteHeader(http.StatusNoContent)
	}))

	statePath := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(statePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReporter(ctx, reporterConfig{
			URL:        server.URL + reportPath,
			ActorName:  "cw-testactor",
			StartupID:  "startup",
			StatePath:  statePath,
			PID:        1,
			Interval:   time.Hour,
			BackoffMin: 5 * time.Millisecond,
			BackoffMax: 20 * time.Millisecond,
		})
	}()

	var got Report
	select {
	case got = <-success:
	case <-time.After(3 * time.Second):
		t.Fatal("reporter did not recover from failed reports")
	}
	cancel()
	waitForDone(t, done)

	mu.Lock()
	n := attempts
	mu.Unlock()
	if n != 3 {
		t.Errorf("attempts = %d, want 3 (two failures then one success)", n)
	}
	if got.Counter != 1 {
		t.Errorf("first accepted report counter = %d, want 1", got.Counter)
	}
}

// TestReporterRereadsStateFileEveryReport checks the root-filesystem
// criterion the integration test relies on: every report carries the state
// file's content as it is on disk at that moment, not the content read when
// the process started.
func TestReporterRereadsStateFileEveryReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("first-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	reports := make(chan Report, 64)
	server := newH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case reports <- rep:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReporter(ctx, reporterConfig{
			URL:        server.URL + reportPath,
			ActorName:  "cw-testactor",
			StartupID:  "startup",
			StatePath:  path,
			PID:        1,
			Interval:   20 * time.Millisecond,
			BackoffMin: 5 * time.Millisecond,
			BackoffMax: 20 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		waitForDone(t, done)
	}()

	waitForContent := func(want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case rep := <-reports:
				if rep.FileContent == want {
					return
				}
			case <-deadline:
				t.Fatalf("no report with file content %q within 3s", want)
			}
		}
	}

	waitForContent("first-content")

	if err := os.WriteFile(path, []byte("second-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The write is trimmed on read, so the report carries the bare content.
	waitForContent("second-content")
}

// TestProbeEgress checks both outcomes a report can carry: a target that
// answers the probe path is reachable, and a closed port is not.
func TestProbeEgress(t *testing.T) {
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != probePath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(reachable.Close)

	// Listen and close again so the port is definitely not accepting
	// connections when the probe dials it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	targets := []string{strings.TrimPrefix(reachable.URL, "http://"), unreachable}
	results := probeEgress(context.Background(), newProbeClient(), targets, time.Second)
	if len(results) != len(targets) {
		t.Fatalf("got %d probe results, want %d", len(results), len(targets))
	}
	for i, target := range targets {
		if results[i].Target != target {
			t.Errorf("result %d target = %q, want %q", i, results[i].Target, target)
		}
	}
	if !results[0].OK || results[0].Error != "" {
		t.Errorf("reachable target result = %+v, want OK with no error", results[0])
	}
	if results[1].OK || results[1].Error == "" {
		t.Errorf("unreachable target result = %+v, want a failure with an error", results[1])
	}
}

// TestReporterCarriesEgressProbes checks that every report includes the probe
// results, which is how the integration test learns whether an actor could
// reach the control plane's connector port and extsim.
func TestReporterCarriesEgressProbes(t *testing.T) {
	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != probePath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(probeServer.Close)

	reports := make(chan Report, 8)
	server := newH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case reports <- rep:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	statePath := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(statePath, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(probeServer.URL, "http://")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReporter(ctx, reporterConfig{
			URL:          server.URL + reportPath,
			ActorName:    "cw-testactor",
			StartupID:    "startup",
			StatePath:    statePath,
			PID:          1,
			Interval:     20 * time.Millisecond,
			BackoffMin:   5 * time.Millisecond,
			BackoffMax:   20 * time.Millisecond,
			ProbeTargets: []string{target},
		})
	}()
	defer func() {
		cancel()
		waitForDone(t, done)
	}()

	select {
	case rep := <-reports:
		if len(rep.Probes) != 1 {
			t.Fatalf("report carried %d probes, want 1: %+v", len(rep.Probes), rep.Probes)
		}
		got := rep.Probes[0]
		if got.Target != target || !got.OK || got.Error != "" {
			t.Errorf("probe result = %+v, want target %q reachable", got, target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no report arrived")
	}
}

func TestReadyzHandler(t *testing.T) {
	tests := []struct {
		name   string
		actor  string
		wantOK bool
	}{
		{name: "ready", actor: "cw-abc123", wantOK: true},
		{name: "crash", actor: "cw-crashabc123", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(readyzHandler(tc.actor))
			defer srv.Close()

			for i := 0; i < 3; i++ {
				resp, err := http.Get(srv.URL + readyPath)
				if err != nil {
					t.Fatalf("GET %s: %v", readyPath, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if tc.wantOK && resp.StatusCode != http.StatusOK {
					t.Fatalf("attempt %d status = %d, want 200", i, resp.StatusCode)
				}
				if !tc.wantOK && resp.StatusCode == http.StatusOK {
					t.Fatalf("attempt %d status = 200, crash actor must never be ready", i)
				}
			}
		})
	}
}

func TestReadActorName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, actorNameFile)
	if err := os.WriteFile(path, []byte("cw-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readActorName(dir)
	if err != nil {
		t.Fatalf("readActorName: %v", err)
	}
	if got != "cw-abc123" {
		t.Errorf("actor name = %q, want %q", got, "cw-abc123")
	}

	if _, err := readActorName(t.TempDir()); err == nil {
		t.Error("readActorName on a directory without actor-name returned no error")
	}
}

func TestEnsureStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providertest", "state")
	first, err := ensureStateFile(path)
	if err != nil {
		t.Fatalf("ensureStateFile: %v", err)
	}
	if first == "" {
		t.Fatal("state file content is empty")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if string(onDisk) != first {
		t.Errorf("state file content = %q, want %q", string(onDisk), first)
	}

	second, err := ensureStateFile(path)
	if err != nil {
		t.Fatalf("second ensureStateFile: %v", err)
	}
	if second != first {
		t.Errorf("second read = %q, want the existing %q", second, first)
	}

	other, err := ensureStateFile(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("ensureStateFile on a fresh path: %v", err)
	}
	if other == first {
		t.Error("two fresh state files got the same random content")
	}
}
