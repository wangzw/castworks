//go:build integration

// The integration tests exercise the provider against the development
// cluster's castworks-dev atespace, following
// docs/milestone/v0.1.0/sandboxprovider.md "测试" and the v0.2.0 changes in
// docs/milestone/v0.2.0/sandboxprovider.md "测试". They need an actor image
// built by hack/providertest/build.sh and a reachable cluster; the commands
// and prerequisites are in the milestone document and deploy/dev/README.md.
//
// The provider test image reports to the control-plane address as cleartext
// HTTP/2 with prior knowledge. The tests serve those reports from the host
// address the castworks-dev Service points at, so an actor reaches the test
// process through the same path the warden will use.

package substrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

const (
	integrationAtespace       = "castworks-dev"
	integrationControlPlane   = "castworks-dev.castworks-system.svc.cluster.local:9090"
	integrationConnector      = "castworks-dev.castworks-system.svc.cluster.local:9091"
	integrationServiceAccount = "castworks-system/sandboxprovider"
	integrationDefaultListen  = "10.12.72.11:9090"
	integrationReportPath     = "/report"

	// integrationOtherConnector differs from integrationConnector in the port
	// only, so a test can change nothing but Config.Connector. Nothing dials it;
	// it exists to give the content-derived template name a different input.
	integrationOtherConnector = "castworks-dev.castworks-system.svc.cluster.local:19091"

	integrationReportBodyLimit = 1 << 20
)

// integrationReport is the body hack/providertest posts for every tick.
type integrationReport struct {
	ActorName   string `json:"actor_name"`
	StartupID   string `json:"startup_id"`
	Counter     int64  `json:"counter"`
	FileContent string `json:"file_content"`
	PID         int    `json:"pid"`
}

// integrationState is built once per test binary: it holds the provider
// Config, the report server and enough information to clean up a template
// this run created.
type integrationState struct {
	cfg             Config
	templateName    string
	templateExisted bool
	hub             *reportHub
	server          *http.Server
}

var (
	integrationOnce    sync.Once
	integrationCurrent *integrationState
	integrationErr     error
)

// integrationFor returns the shared state, failing the test when the
// environment is incomplete. CASTWORKS_IT_IMAGE is required; the tests never
// skip.
func integrationFor(t *testing.T) *integrationState {
	t.Helper()
	integrationOnce.Do(func() {
		integrationCurrent, integrationErr = newIntegrationState()
	})
	if integrationErr != nil {
		t.Fatal(integrationErr)
	}
	return integrationCurrent
}

func newIntegrationState() (*integrationState, error) {
	image := os.Getenv("CASTWORKS_IT_IMAGE")
	if image == "" {
		return nil, errors.New("CASTWORKS_IT_IMAGE is not set; pass the reference produced by hack/providertest/build.sh")
	}
	listen := os.Getenv("CASTWORKS_IT_LISTEN")
	if listen == "" {
		listen = integrationDefaultListen
	}
	cfg := Config{
		KubeContext:    os.Getenv("CASTWORKS_IT_KUBE_CONTEXT"),
		Atespace:       integrationAtespace,
		Image:          image,
		ControlPlane:   integrationControlPlane,
		Connector:      integrationConnector,
		ServiceAccount: integrationServiceAccount,
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	template, err := buildActorTemplate(cfg.Atespace, cfg.Image, cfg.ControlPlane, cfg.Connector)
	if err != nil {
		return nil, err
	}
	templateName, err := actorTemplateName(template)
	if err != nil {
		return nil, err
	}

	hub := newReportHub()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for actor reports: %w; the control plane may already be using port 9090", listen, err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Handler:           hub,
		Protocols:         &protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Warn("actor report server stopped", "error", err)
		}
	}()

	existed, err := templateExists(cfg, templateName)
	if err != nil {
		_ = server.Close()
		return nil, err
	}

	return &integrationState{
		cfg:             cfg,
		templateName:    templateName,
		templateExisted: existed,
		hub:             hub,
		server:          server,
	}, nil
}

// templateExists checks the template without creating it, so the test knows
// whether a template it later deletes was already there.
func templateExists(cfg Config, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, closeClient, err := newAteAPIClient(ctx, ateAPIConnConfig{
		Kubeconfig:     cfg.Kubeconfig,
		KubeContext:    cfg.KubeContext,
		ServiceAccount: cfg.ServiceAccount,
	})
	if err != nil {
		return false, fmt.Errorf("check whether actor template %s exists: %w", name, err)
	}
	defer func() { _ = closeClient() }()

	_, err = client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{
		ActorTemplate: &ateapi.ObjectRef{Atespace: cfg.Atespace, Name: name},
	})
	if err == nil {
		return true, nil
	}
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	return false, fmt.Errorf("get actor template %s/%s: %w", cfg.Atespace, name, err)
}

// teardown stops the report server and deletes the template when this run
// created it and no actor still references it. A pre-existing template is
// always left alone.
func (s *integrationState) teardown() {
	if s == nil {
		return
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	if s.templateExisted {
		return
	}
	deleteTemplateUnlessReferenced(s.cfg, s.templateName)
}

// deleteTemplateUnlessReferenced deletes the named actor template when no
// actor in the atespace still uses it. Substrate has no template update RPC,
// and deleting a template an actor still references breaks the actor's later
// resumes, so a referenced template is left in place with a log line instead.
func deleteTemplateUnlessReferenced(cfg Config, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, closeClient, err := newAteAPIClient(ctx, ateAPIConnConfig{
		Kubeconfig:     cfg.Kubeconfig,
		KubeContext:    cfg.KubeContext,
		ServiceAccount: cfg.ServiceAccount,
	})
	if err != nil {
		slog.Default().Warn("leave the actor template in place: connect to ate-api",
			"template", name, "error", err)
		return
	}
	defer func() { _ = closeClient() }()

	referencing, err := actorReferencingTemplate(ctx, client, cfg.Atespace, name)
	if err != nil {
		slog.Default().Warn("leave the actor template in place: list actors",
			"template", name, "error", err)
		return
	}
	if referencing != "" {
		slog.Default().Info("leave the actor template in place: an actor still references it",
			"template", name, "actor", referencing)
		return
	}

	if _, err := client.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{
		ActorTemplate: &ateapi.ObjectRef{Atespace: cfg.Atespace, Name: name},
	}); err != nil && status.Code(err) != codes.NotFound {
		slog.Default().Warn("delete the actor template", "template", name, "error", err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if integrationCurrent != nil {
		integrationCurrent.teardown()
	}
	os.Exit(code)
}

// reportHub routes actor reports to the subscribers waiting for that actor.
type reportHub struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[string]map[int]chan integrationReport
}

func newReportHub() *reportHub {
	return &reportHub{subscribers: make(map[string]map[int]chan integrationReport)}
}

// subscribe returns a buffered stream of reports for one actor and a function
// that removes it. Reports are dropped rather than blocking the reporter when
// a subscriber falls behind.
func (h *reportHub) subscribe(actor string) (<-chan integrationReport, func()) {
	ch := make(chan integrationReport, 1024)
	h.mu.Lock()
	if h.subscribers[actor] == nil {
		h.subscribers[actor] = make(map[int]chan integrationReport)
	}
	id := h.nextID
	h.nextID++
	h.subscribers[actor][id] = ch
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if subs := h.subscribers[actor]; subs != nil {
			delete(subs, id)
			if len(subs) == 0 {
				delete(h.subscribers, actor)
			}
		}
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

func (h *reportHub) publish(report integrationReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subscribers[report.ActorName] {
		select {
		case ch <- report:
		default:
		}
	}
}

func (h *reportHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != integrationReportPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, integrationReportBodyLimit))
	if err != nil {
		http.Error(w, "read report: "+err.Error(), http.StatusBadRequest)
		return
	}
	var report integrationReport
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, "decode report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if report.ActorName == "" {
		http.Error(w, "report has no actor_name", http.StatusBadRequest)
		return
	}
	h.publish(report)
	w.WriteHeader(http.StatusNoContent)
}

// TestTemplateIsIdempotent shows that a second New reuses the template the
// first one created: the UID and creation time do not change.
func TestTemplateIsIdempotent(t *testing.T) {
	state := integrationFor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	first := state.newProvider(t, 2*time.Minute)
	before, err := state.getTemplate(ctx, first)
	if err != nil {
		t.Fatalf("get actor template after the first New: %v", err)
	}

	second := state.newProvider(t, 2*time.Minute)
	after, err := state.getTemplate(ctx, second)
	if err != nil {
		t.Fatalf("get actor template after the second New: %v", err)
	}

	if before.GetMetadata().GetUid() != after.GetMetadata().GetUid() {
		t.Errorf("actor template UID changed from %q to %q; the second New recreated it",
			before.GetMetadata().GetUid(), after.GetMetadata().GetUid())
	}
	beforeTime, afterTime := before.GetMetadata().GetCreateTime(), after.GetMetadata().GetCreateTime()
	if beforeTime == nil || afterTime == nil {
		t.Fatalf("actor template create_time is unset: before=%v after=%v", beforeTime, afterTime)
	}
	if !beforeTime.AsTime().Equal(afterTime.AsTime()) {
		t.Errorf("actor template create_time changed from %v to %v; the second New recreated it",
			beforeTime.AsTime(), afterTime.AsTime())
	}
}

// TestTemplateNameFollowsConnector shows that changing only Config.Connector
// yields a different content-derived template name, and that a provider built
// from that configuration creates the other template. The template this test
// creates is deleted after the provider closes; one that already existed is
// left alone.
func TestTemplateNameFollowsConnector(t *testing.T) {
	state := integrationFor(t)

	other := state.cfg
	other.Connector = integrationOtherConnector

	template, err := buildActorTemplate(other.Atespace, other.Image, other.ControlPlane, other.Connector)
	if err != nil {
		t.Fatalf("buildActorTemplate: %v", err)
	}
	otherName, err := actorTemplateName(template)
	if err != nil {
		t.Fatalf("actorTemplateName: %v", err)
	}
	if otherName == state.templateName {
		t.Fatalf("template name %q is unchanged after changing only Connector from %q to %q; the name does not track the content",
			otherName, state.cfg.Connector, other.Connector)
	}

	existed, err := templateExists(other, otherName)
	if err != nil {
		t.Fatalf("check whether actor template %s exists: %v", otherName, err)
	}
	if !existed {
		// Registered before the provider is built so that the provider closes
		// first: cleanups run in reverse order.
		t.Cleanup(func() { deleteTemplateUnlessReferenced(other, otherName) })
	}

	p := newProviderFor(t, other, 2*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, err := p.client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{
		ActorTemplate: &ateapi.ObjectRef{Atespace: other.Atespace, Name: otherName},
	})
	if err != nil {
		t.Fatalf("get actor template %s: %v", otherName, err)
	}
	if name := got.GetMetadata().GetName(); name != otherName {
		t.Errorf("actor template metadata name = %q, want %q", name, otherName)
	}
	if value := templateEnvValue(got, templateContainerName, templateConnector); value != other.Connector {
		t.Errorf("actor template %s environment variable %s = %q, want %q",
			otherName, templateConnector, value, other.Connector)
	}
}

// templateEnvValue returns the value of the named environment variable on the
// named container, or "" when either is missing.
func templateEnvValue(tpl *ateapi.ActorTemplate, container, env string) string {
	for _, c := range tpl.GetContainers() {
		if c.GetName() != container {
			continue
		}
		for _, e := range c.GetEnv() {
			if e.GetName() == env {
				return e.GetValue()
			}
		}
	}
	return ""
}

// TestFullCycle provisions an actor, receives its reports, snapshots and
// restores it, and checks that the restored process kept its startup ID, its
// file and a growing counter. It then destroys the actor twice and checks that
// GetSandbox reports it missing.
func TestFullCycle(t *testing.T) {
	state := integrationFor(t)
	runID := randomRunID(t, "it")
	actor := sandboxprovider.ActorName(runID)

	reports, unsubscribe := state.hub.subscribe(actor)
	t.Cleanup(unsubscribe)

	p := state.newProvider(t, 2*time.Minute)
	destroyOnCleanup(t, p, actor)

	provisionCtx, cancelProvision := context.WithTimeout(context.Background(), 5*time.Minute)
	provision, err := p.Provision(provisionCtx, runID)
	cancelProvision()
	if err != nil {
		t.Fatalf("Provision(%s): %v", runID, err)
	}

	first := waitForReport(t, reports, 3*time.Minute, func(integrationReport) bool { return true })
	if first.ActorName != actor {
		t.Fatalf("first report actor name = %q, want %q", first.ActorName, actor)
	}

	retryCtx, cancelRetry := context.WithTimeout(context.Background(), 3*time.Minute)
	_, retryErr := p.Provision(retryCtx, runID)
	cancelRetry()
	if retryErr != nil {
		t.Fatalf("second Provision(%s): %v", runID, retryErr)
	}

	waitForState(t, p, actor, sandboxprovider.ActorStateRunning, 30*time.Second)

	snapshotCtx, cancelSnapshot := context.WithTimeout(context.Background(), 5*time.Minute)
	snapshot, err := p.Snapshot(snapshotCtx, actor)
	cancelSnapshot()
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", actor, err)
	}

	// The synchronous suspend has returned and the process is frozen. Take the
	// last report before the snapshot as the state the restore must preserve.
	before, found := lastReportAfterQuiet(reports, 1500*time.Millisecond, 30*time.Second)
	if !found {
		before = first
	}

	snapshotURI := waitForSnapshotURI(t, p, state.cfg.Atespace, actor, 2*time.Minute)
	waitForState(t, p, actor, sandboxprovider.ActorStateSuspended, 30*time.Second)

	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 5*time.Minute)
	restore, err := p.RestoreSnapshot(restoreCtx, actor)
	cancelRestore()
	if err != nil {
		t.Fatalf("RestoreSnapshot(%s): %v", actor, err)
	}

	// Waiting for a counter above the pre-suspend one also covers a report the
	// server accepted but whose response the actor did not receive before the
	// snapshot: the following report clears the threshold.
	after := waitForReport(t, reports, 3*time.Minute, func(r integrationReport) bool {
		return r.Counter > before.Counter
	})
	if after.ActorName != actor {
		t.Errorf("restored report actor name = %q, want %q", after.ActorName, actor)
	}
	if after.StartupID != before.StartupID {
		t.Errorf("restored startup ID = %q, want %q; the process did not survive the snapshot",
			after.StartupID, before.StartupID)
	}
	if after.FileContent != before.FileContent {
		t.Errorf("restored file content = %q, want %q; the root filesystem did not survive the snapshot",
			after.FileContent, before.FileContent)
	}

	destroyCtx, cancelDestroy := context.WithTimeout(context.Background(), 2*time.Minute)
	err = p.Destroy(destroyCtx, actor)
	cancelDestroy()
	if err != nil {
		t.Fatalf("Destroy(%s): %v", actor, err)
	}
	destroyAgainCtx, cancelDestroyAgain := context.WithTimeout(context.Background(), 2*time.Minute)
	err = p.Destroy(destroyAgainCtx, actor)
	cancelDestroyAgain()
	if err != nil {
		t.Fatalf("second Destroy(%s): %v", actor, err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		getCtx, cancelGet := context.WithTimeout(context.Background(), 30*time.Second)
		status, err := p.GetSandbox(getCtx, actor)
		cancelGet()
		if errors.Is(err, sandboxprovider.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetSandbox(%s) after Destroy = %v, %v; want ErrNotFound", actor, status.State, err)
		}
		time.Sleep(2 * time.Second)
	}

	t.Logf("timing provision=%s snapshot=%s restore=%s snapshot_uri=%s",
		provision.Duration, snapshot.Duration, restore.Duration, snapshotURI)
}

// TestCrash provisions an actor whose wakeup probe never passes. ResumeActor
// times out, Substrate marks the actor CRASHED, and Provision reports it as
// ErrActorCrashed.
func TestCrash(t *testing.T) {
	state := integrationFor(t)
	runID := randomRunID(t, "crash")
	actor := sandboxprovider.ActorName(runID)

	p := state.newProvider(t, 2*time.Minute)
	destroyOnCleanup(t, p, actor)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	if _, err := p.Provision(ctx, runID); !errors.Is(err, sandboxprovider.ErrActorCrashed) {
		t.Fatalf("Provision(%s) error = %v, want ErrActorCrashed", runID, err)
	}
	waitForState(t, p, actor, sandboxprovider.ActorStateCrashed, time.Minute)
	if err := p.Destroy(ctx, actor); err != nil {
		t.Fatalf("Destroy(%s): %v", actor, err)
	}
}

// newProvider builds a provider for the shared Config and closes it at the end
// of the test.
func (s *integrationState) newProvider(t *testing.T, timeout time.Duration) *Provider {
	t.Helper()
	return newProviderFor(t, s.cfg, timeout)
}

// newProviderFor builds a provider for cfg and closes it at the end of the
// test. A test that varies one Config field uses it without changing the
// shared state.
func newProviderFor(t *testing.T, cfg Config, timeout time.Duration) *Provider {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	p, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

func (s *integrationState) getTemplate(ctx context.Context, p *Provider) (*ateapi.ActorTemplate, error) {
	return p.client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{
		ActorTemplate: &ateapi.ObjectRef{Atespace: s.cfg.Atespace, Name: s.templateName},
	})
}

// destroyOnCleanup removes the actor with a fresh context, independent of the
// test's cancellation, before the provider is closed.
func destroyOnCleanup(t *testing.T, p *Provider, actor string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := p.Destroy(ctx, actor); err != nil {
			t.Errorf("Destroy(%s) during cleanup: %v", actor, err)
		}
	})
}

func randomRunID(t *testing.T, prefix string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate a random run ID: %v", err)
	}
	return prefix + hex.EncodeToString(b[:])
}

func waitForReport(t *testing.T, reports <-chan integrationReport, timeout time.Duration, match func(integrationReport) bool) integrationReport {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case report := <-reports:
			if match(report) {
				return report
			}
		case <-deadline:
			t.Fatalf("no matching actor report within %s", timeout)
			return integrationReport{}
		}
	}
}

// lastReportAfterQuiet returns the last report received before a quiet gap of
// the given length, waiting at most max for one to arrive.
func lastReportAfterQuiet(reports <-chan integrationReport, quiet, max time.Duration) (integrationReport, bool) {
	deadline := time.Now().Add(max)
	var last integrationReport
	found := false
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last, found
		}
		timer := time.NewTimer(min(remaining, quiet))
		select {
		case report := <-reports:
			timer.Stop()
			last, found = report, true
		case <-timer.C:
			if found {
				return last, true
			}
		}
	}
}

func waitForState(t *testing.T, p *Provider, actor string, want sandboxprovider.ActorState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var (
		last    sandboxprovider.SandboxStatus
		lastErr error
	)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		last, lastErr = p.GetSandbox(ctx, actor)
		cancel()
		if lastErr == nil && last.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetSandbox(%s) = %v, %v; want %s within %s", actor, last.State, lastErr, want, timeout)
		}
		time.Sleep(time.Second)
	}
}

// waitForSnapshotURI returns the snapshot URI recorded for the actor's latest
// durable snapshot, polling because the status can trail the synchronous
// suspend by a moment.
func waitForSnapshotURI(t *testing.T, p *Provider, atespace, actor string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a, err := p.client.GetActor(ctx, &ateapi.GetActorRequest{
			Actor: &ateapi.ObjectRef{Atespace: atespace, Name: actor},
		})
		cancel()
		if err != nil {
			t.Fatalf("get actor %s for its snapshot URI: %v", actor, err)
		}
		if uri := latestSnapshotURI(a); uri != "" {
			return uri
		}
		if time.Now().After(deadline) {
			t.Fatalf("actor %s has no durable snapshot URI within %s", actor, timeout)
		}
		time.Sleep(time.Second)
	}
}

func latestSnapshotURI(a *ateapi.Actor) string {
	var (
		uri        string
		generation int32
	)
	for _, snapshot := range a.GetStatus().GetSnapshots() {
		if snapshot.GetGeneration() < generation {
			continue
		}
		if got := snapshot.GetDurableSnapshot().GetObject().GetSnapshotUri(); got != "" {
			uri = got
			generation = snapshot.GetGeneration()
		}
	}
	return uri
}
