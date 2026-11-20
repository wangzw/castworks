// Command providertest runs inside a Substrate actor during the
// sandboxprovider integration tests. It stands in for the warden process so
// the tests can check what a snapshot preserves without depending on the real
// actor image.
//
// The program:
//
//   - answers the wakeup probe on port 8081 at /readyz;
//   - generates a startup ID that lives only in memory;
//   - reads its actor name from /run/ate/actor-name;
//   - writes a file with random content to the root filesystem on the first
//     start and re-reads it before every report;
//   - probes the connector endpoint and extsim by name through the egress
//     gateway before every report;
//   - POSTs a report to CASTWORKS_CONTROL_PLANE every two seconds over
//     cleartext HTTP/2 with prior knowledge.
//
// A report is a JSON object with these fields:
//
//	{"actor_name":"cw-...","startup_id":"<hex>","counter":1,
//	 "file_content":"<hex>","pid":1,
//	 "probes":[{"target":"host:port","ok":true}]}
//
// The counter only grows while the process keeps its memory, so a report after
// RestoreSnapshot with the same startup ID, the same file content and a larger
// counter proves that the snapshot restored the process state. Failed reports
// are retried with a backoff, because a new actor's EgressPolicy takes a few
// seconds to propagate.
//
// An actor whose name starts with "cw-crash" never answers /readyz with 200.
// ResumeActor then hits its wakeup probe timeout and Substrate marks the actor
// CRASHED, which the integration test expects.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	readyAddr = ":8081"
	readyPath = "/readyz"

	// defaultActorDir is where the actor template mounts the system-info
	// volume; the file holds the actor name Substrate assigned.
	defaultActorDir = "/run/ate"
	actorNameFile   = "actor-name"

	// defaultStatePath is on the root filesystem, not /tmp: the template's
	// MEMORY snapshot fidelity preserves root filesystem writes.
	defaultStatePath = "/var/lib/providertest/state"

	// reportPath is the endpoint the integration test server serves.
	reportPath = "/report"

	// probePath is served by the integration test's probe listeners, which
	// stand in for the control plane's connector endpoint and extsim.
	probePath = "/probe"

	// defaultExtsimTarget is the development environment's extsim address. The
	// actor template carries the control-plane and connector addresses only, so
	// the probe target for extsim is fixed here; the integration test uses the
	// same name in its egress configuration.
	defaultExtsimTarget = "extsim.castworks-system.svc.cluster.local:9100"

	// crashActorPrefix marks an actor that must fail its wakeup probe.
	crashActorPrefix = "cw-crash"

	defaultReportInterval = 2 * time.Second
	defaultBackoffMin     = 1 * time.Second
	defaultBackoffMax     = 10 * time.Second
	defaultReportTimeout  = 10 * time.Second
	defaultProbeTimeout   = 2 * time.Second
)

// Probe is the result of one egress probe. Target is the host:port the probe
// dialed by name through the egress gateway.
type Probe struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// Report is the JSON body of one report POST.
type Report struct {
	ActorName   string  `json:"actor_name"`
	StartupID   string  `json:"startup_id"`
	Counter     int64   `json:"counter"`
	FileContent string  `json:"file_content"`
	PID         int     `json:"pid"`
	Probes      []Probe `json:"probes,omitempty"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	actorName, err := readActorName(defaultActorDir)
	if err != nil {
		log.Fatalf("providertest: read actor name: %v", err)
	}
	if _, err := ensureStateFile(defaultStatePath); err != nil {
		log.Fatalf("providertest: state file: %v", err)
	}
	startupID, err := randomHex(16)
	if err != nil {
		log.Fatalf("providertest: startup id: %v", err)
	}
	controlPlane := os.Getenv("CASTWORKS_CONTROL_PLANE")
	if controlPlane == "" {
		log.Fatal("providertest: CASTWORKS_CONTROL_PLANE is not set")
	}
	// The connector endpoint shares the control-plane host and runs on 9091.
	// The template always sets it, but an empty value only drops the probe.
	var probeTargets []string
	if connector := os.Getenv("CASTWORKS_CONNECTOR"); connector != "" {
		probeTargets = append(probeTargets, connector)
	}
	probeTargets = append(probeTargets, defaultExtsimTarget)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go serveReady(ctx, readyAddr, actorName)

	log.Printf("providertest: actor=%s startup=%s control_plane=%s probes=%v crash=%t",
		actorName, startupID, controlPlane, probeTargets, strings.HasPrefix(actorName, crashActorPrefix))

	runReporter(ctx, reporterConfig{
		URL:          "http://" + controlPlane + reportPath,
		ActorName:    actorName,
		StartupID:    startupID,
		StatePath:    defaultStatePath,
		PID:          os.Getpid(),
		Client:       newH2CClient(),
		ProbeTargets: probeTargets,
	})
}

// reporterConfig carries everything runReporter needs. The zero timeouts fall
// back to the defaults so tests only override what they care about.
type reporterConfig struct {
	URL          string
	ActorName    string
	StartupID    string
	StatePath    string
	PID          int
	Interval     time.Duration
	BackoffMin   time.Duration
	BackoffMax   time.Duration
	Client       *http.Client
	ProbeTargets []string
	ProbeClient  *http.Client
	ProbeTimeout time.Duration
}

func (c reporterConfig) withDefaults() reporterConfig {
	if c.Interval <= 0 {
		c.Interval = defaultReportInterval
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = defaultBackoffMin
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = defaultBackoffMax
	}
	if c.Client == nil {
		c.Client = newH2CClient()
	}
	if c.ProbeClient == nil {
		c.ProbeClient = newProbeClient()
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = defaultProbeTimeout
	}
	return c
}

// runReporter sends one report at a time and returns when ctx is cancelled.
// The counter advances only after a report is accepted, so the first accepted
// report always carries counter 1 and a restored process continues from the
// value it had before the snapshot.
func runReporter(ctx context.Context, cfg reporterConfig) {
	cfg = cfg.withDefaults()
	backoff := cfg.BackoffMin
	var counter int64
	for {
		next := counter + 1

		// Read the state file again before every report. The report must
		// describe the root filesystem as it is now, not as it was when the
		// process started; the integration test compares the reported content
		// across a snapshot.
		content, err := readStateFile(cfg.StatePath)
		if err == nil {
			err = sendReport(ctx, cfg.Client, cfg.URL, Report{
				ActorName:   cfg.ActorName,
				StartupID:   cfg.StartupID,
				Counter:     next,
				FileContent: content,
				PID:         cfg.PID,
				Probes:      probeEgress(ctx, cfg.ProbeClient, cfg.ProbeTargets, cfg.ProbeTimeout),
			})
		} else {
			err = fmt.Errorf("read state file: %w", err)
		}

		wait := cfg.Interval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("providertest: report %d failed: %v; retrying in %s", next, err, backoff)
			wait = backoff
			backoff *= 2
			if backoff > cfg.BackoffMax {
				backoff = cfg.BackoffMax
			}
		} else {
			counter = next
			backoff = cfg.BackoffMin
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// newH2CClient returns an HTTP client that speaks cleartext HTTP/2 with prior
// knowledge, as the EgressPolicy's http rule allows.
func newH2CClient() *http.Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{Protocols: &protocols},
		Timeout:   defaultReportTimeout,
	}
}

// newProbeClient returns the HTTP/1.1 client the egress probes use. The egress
// gateway evaluates an http rule per request, so either HTTP version works;
// HTTP/1.1 keeps the probe servers free of an h2c setup.
func newProbeClient() *http.Client {
	return &http.Client{Timeout: defaultProbeTimeout}
}

// probeEgress probes every target concurrently and returns the results in the
// order of targets. Probes run before every report so a target that becomes
// reachable after the egress policy propagates shows up in a later report.
func probeEgress(ctx context.Context, client *http.Client, targets []string, timeout time.Duration) []Probe {
	if len(targets) == 0 {
		return nil
	}
	results := make([]Probe, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = probeTarget(ctx, client, target, timeout)
		}()
	}
	wg.Wait()
	return results
}

// probeTarget sends one cleartext HTTP GET to target by name. Any 2xx answer
// counts as reachable. A failure is recorded, not logged: the report carries
// it back to the test.
func probeTarget(ctx context.Context, client *http.Client, target string, timeout time.Duration) Probe {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+target+probePath, nil)
	if err != nil {
		return Probe{Target: target, Error: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Probe{Target: target, Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Probe{Target: target, Error: "target answered " + resp.Status}
	}
	return Probe{Target: target, OK: true}
}

func sendReport(ctx context.Context, client *http.Client, url string, rep Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("control plane answered %s", resp.Status)
	}
	return nil
}

// serveReady serves the wakeup probe until ctx is cancelled. Without this
// listener ResumeActor can never succeed, so a bind failure ends the process.
func serveReady(ctx context.Context, addr, actorName string) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           readyzHandler(actorName),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("providertest: readyz listener: %v", err)
	}
}

// readyzHandler reports ready unless actorName selects the crash mode, where
// it must never answer 200 so the wakeup probe times out.
func readyzHandler(actorName string) http.Handler {
	crash := strings.HasPrefix(actorName, crashActorPrefix)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readyPath {
			http.NotFound(w, r)
			return
		}
		if crash {
			http.Error(w, "crash actor never answers its wakeup probe", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
}

// readActorName reads and trims the actor name Substrate wrote into the
// system-info mount.
func readActorName(dir string) (string, error) {
	path := filepath.Join(dir, actorNameFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return name, nil
}

// ensureStateFile returns the content of the state file, creating it with
// random content when it does not exist yet. A restored process finds the file
// it wrote before the snapshot.
func ensureStateFile(path string) (string, error) {
	content, err := readStateFile(path)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	content, err = randomHex(16)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return content, nil
}

// readStateFile returns the state file's trimmed content. An empty file is an
// error because an empty report would not prove anything about persistence.
func readStateFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return content, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
