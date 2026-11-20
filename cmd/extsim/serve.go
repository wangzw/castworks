package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Defaults from docs/milestone/v0.2.0/extsim.md "运行".
const (
	serveDefaultListen        = "0.0.0.0:9100"
	serveDefaultControlListen = "127.0.0.1:9101"
	serveDefaultCallbackURL   = "http://127.0.0.1:9092" + callbackWebhookPath
	serveDefaultJobDuration   = 60 * time.Second

	// serveReadHeaderTimeout bounds the time a client may take to send request
	// headers on either listener.
	serveReadHeaderTimeout = 5 * time.Second
)

// callbackWebhookPath is the path connectorhub serves its webhook receiver on.
// It is part of the external system definition in
// deploy/dev/connectorhub/extsim.yaml.
const callbackWebhookPath = "/hooks/extsim"

// serveConfig is the parsed form of the extsim serve flags.
type serveConfig struct {
	Listen         string
	ControlListen  string
	CallbackURL    string
	SigningKeyFile string
	JobDuration    time.Duration
}

// parseServeConfig parses and validates the serve flags.
func parseServeConfig(args []string, stderr io.Writer) (serveConfig, error) {
	cfg := serveConfig{
		Listen:        serveDefaultListen,
		ControlListen: serveDefaultControlListen,
		CallbackURL:   serveDefaultCallbackURL,
		JobDuration:   serveDefaultJobDuration,
	}

	fs := flag.NewFlagSet("extsim serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "job interface listen address")
	fs.StringVar(&cfg.ControlListen, "control-listen", cfg.ControlListen, "test-control interface listen address; must be a loopback address")
	fs.StringVar(&cfg.CallbackURL, "callback-url", cfg.CallbackURL, "URL callbacks are posted to")
	fs.StringVar(&cfg.SigningKeyFile, "signing-key-file", "", "file holding the HMAC signing key (required)")
	fs.DurationVar(&cfg.JobDuration, "job-duration", cfg.JobDuration, "default job duration when no plan sets one")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	if fs.NArg() != 0 {
		return serveConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	if cfg.SigningKeyFile == "" {
		return serveConfig{}, errors.New("--signing-key-file is required")
	}
	if cfg.Listen == "" {
		return serveConfig{}, errors.New("--listen must not be empty")
	}
	if cfg.JobDuration <= 0 {
		return serveConfig{}, fmt.Errorf("--job-duration must be positive, got %s", cfg.JobDuration)
	}
	if err := validateControlListen(cfg.ControlListen); err != nil {
		return serveConfig{}, err
	}
	if err := validateCallbackURL(cfg.CallbackURL); err != nil {
		return serveConfig{}, err
	}
	return cfg, nil
}

// validateControlListen rejects anything but a loopback address, because the
// test-control interface can make jobs complete early and drop callbacks.
func validateControlListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --control-listen %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("--control-listen %q is not a loopback address; bind to 127.0.0.1 or ::1", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("--control-listen %q is not a loopback address; bind to 127.0.0.1 or ::1", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	return fmt.Errorf("--control-listen %q is not a loopback address; bind to 127.0.0.1 or ::1", addr)
}

// validateCallbackURL checks that the callback target is an absolute http or
// https URL.
func validateCallbackURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid --callback-url %q: %w", raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid --callback-url %q: want an http or https URL", raw)
	}
	return nil
}

// runServe starts the service and runs it until a signal arrives or a listener
// fails. It returns the process exit code.
func runServe(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseServeConfig(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "extsim: %v\n", err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, cfg, logger); err != nil {
		_, _ = fmt.Fprintf(stderr, "extsim: %v\n", err)
		return 1
	}
	return 0
}

// serve binds both listeners, serves them and shuts down when ctx ends. Closing
// the listeners drops pending callbacks, matching the restart semantics in the
// milestone document.
func serve(ctx context.Context, cfg serveConfig, logger *slog.Logger) error {
	if _, err := os.ReadFile(cfg.SigningKeyFile); err != nil {
		return fmt.Errorf("read signing key file: %w", err)
	}

	clock := RealClock()
	jobs := NewJobService(clock, cfg.JobDuration)
	sender := newCallbackSender(cfg.CallbackURL, cfg.SigningKeyFile, clock, logger)
	defer sender.close()
	assembly := newServiceAssembly(jobs, sender, logger)

	jobsListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on job interface %s: %w", cfg.Listen, err)
	}
	controlListener, err := net.Listen("tcp", cfg.ControlListen)
	if err != nil {
		_ = jobsListener.Close()
		return fmt.Errorf("listen on control interface %s: %w", cfg.ControlListen, err)
	}

	logger.Info("listening", "listener", "jobs", "addr", jobsListener.Addr().String())
	logger.Info("listening", "listener", "control", "addr", controlListener.Addr().String())

	jobsServer := &http.Server{Handler: assembly.jobsHandler(), ReadHeaderTimeout: serveReadHeaderTimeout}
	controlServer := &http.Server{Handler: assembly.controlHandler(), ReadHeaderTimeout: serveReadHeaderTimeout}

	serveErrs := make(chan error, 2)
	go func() { serveErrs <- jobsServer.Serve(jobsListener) }()
	go func() { serveErrs <- controlServer.Serve(controlListener) }()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serveErrs:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}

	_ = jobsServer.Close()
	_ = controlServer.Close()
	return serveErr
}

// serviceAssembly wires the job service to the callback sender and owns the
// plan queue and the test-control interface.
type serviceAssembly struct {
	svc    *JobService
	sender *callbackSender
	logger *slog.Logger

	// submitMu serializes plan consumption with job submission. The queued
	// status hook runs synchronously inside JobService.Submit on the submitting
	// goroutine, while that goroutine still holds submitMu, so it can hand the
	// pending plan to the job without a second lock. Nothing else touches
	// pending.
	submitMu sync.Mutex
	pending  *jobPlan

	plansMu sync.Mutex
	plans   []jobPlan

	jobPlansMu sync.Mutex
	jobPlans   map[string]jobPlan
}

// newServiceAssembly returns an assembly and installs its job hooks.
func newServiceAssembly(svc *JobService, sender *callbackSender, logger *slog.Logger) *serviceAssembly {
	if logger == nil {
		logger = slog.Default()
	}
	a := &serviceAssembly{
		svc:      svc,
		sender:   sender,
		logger:   logger,
		jobPlans: make(map[string]jobPlan),
	}
	svc.SetOnStatusChange(a.onStatusChange)
	svc.SetOnComplete(a.onComplete)
	return a
}

// jobsHandler serves the job interface. POST /v1/jobs is handled by the
// assembly so the next plan can set the job duration; every other request goes
// to the job service's own handler.
func (a *serviceAssembly) jobsHandler() http.Handler {
	inner := a.svc.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" {
			a.handleSubmit(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// handleSubmit mirrors JobService.handleSubmit but routes through submit, so a
// plan applies to jobs submitted over HTTP.
func (a *serviceAssembly) handleSubmit(w http.ResponseWriter, r *http.Request) {
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

	job, err := a.submit(req.Project, req.Content)
	switch {
	case err == nil:
		writeJobJSON(w, http.StatusCreated, job)
	case errors.Is(err, ErrJobProjectEmpty), errors.Is(err, ErrJobContentTooLarge):
		writeJobError(w, http.StatusBadRequest, err.Error())
	default:
		writeJobError(w, http.StatusInternalServerError, "internal error")
	}
}

// submit validates the job input, then takes the next plan and submits the job.
// Validation runs first so that a rejected request never consumes a plan.
func (a *serviceAssembly) submit(project, content string) (Job, error) {
	if project == "" {
		return Job{}, ErrJobProjectEmpty
	}
	if len(content) > maxJobContentBytes {
		return Job{}, ErrJobContentTooLarge
	}

	a.submitMu.Lock()
	defer a.submitMu.Unlock()
	plan := a.popPlan()
	a.pending = plan
	var duration time.Duration
	if plan != nil {
		duration = plan.Duration
	}
	job, err := a.svc.Submit(project, content, duration)
	if err != nil {
		a.pending = nil
		return Job{}, err
	}
	return job, nil
}

// onStatusChange logs every status change and attaches the plan waiting for
// this submission to the newly queued job.
func (a *serviceAssembly) onStatusChange(job Job) {
	if job.Status == JobStatusQueued {
		a.attachPendingPlan(job.ID)
	}

	args := []any{"job", job.ID, "status", job.Status}
	if job.Conclusion != "" {
		args = append(args, "conclusion", job.Conclusion)
	}
	if job.Reason != "" {
		args = append(args, "reason", job.Reason)
	}
	a.logger.Info("job status", args...)
}

// attachPendingPlan stores the pending plan under the job identifier. It is
// called from the queued status hook, on the goroutine that holds submitMu.
func (a *serviceAssembly) attachPendingPlan(id string) {
	if a.pending == nil {
		return
	}
	plan := *a.pending
	a.pending = nil
	a.jobPlansMu.Lock()
	defer a.jobPlansMu.Unlock()
	a.jobPlans[id] = plan
}

// onComplete sends the job's callback using the plan attached when it was
// submitted, falling back to a normal delivery.
func (a *serviceAssembly) onComplete(job Job) {
	a.jobPlansMu.Lock()
	plan, ok := a.jobPlans[job.ID]
	delete(a.jobPlans, job.ID)
	a.jobPlansMu.Unlock()
	if !ok {
		plan = jobPlan{Callback: callbackPlan{Callback: callbackDeliver}}
	}

	body, err := json.Marshal(struct {
		Job Job `json:"job"`
	}{Job: job})
	if err != nil {
		a.logger.Error("encode callback body", "job", job.ID, "error", err)
		return
	}
	a.sender.send(job.ID, body, plan.Callback)
}

// jobPlanFor returns the plan attached to a job. It exists for tests.
func (a *serviceAssembly) jobPlanFor(id string) (jobPlan, bool) {
	a.jobPlansMu.Lock()
	defer a.jobPlansMu.Unlock()
	plan, ok := a.jobPlans[id]
	return plan, ok
}
