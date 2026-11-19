package substrate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

const (
	providerTestImage    = "registry.zhanwei.wang/castworks/providertest@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	providerTestTemplate = "cw-0123456789ab"
)

func providerTestConfig() Config {
	return Config{
		Atespace:       "castworks-dev",
		Image:          providerTestImage,
		ControlPlane:   "castworks-dev.castworks-system.svc.cluster.local:9090",
		ServiceAccount: "castworks-system/sandboxprovider",
	}
}

// providerTestDeps records the constructor's calls and lets each test decide
// where the fake connection or the fake template step fails.
type providerTestDeps struct {
	newClientCalls      atomic.Int32
	ensureTemplateCalls atomic.Int32
	closeCalls          atomic.Int32

	client            ateapi.ControlClient
	newClientErr      error
	ensureTemplateErr error
	template          string
	closeErr          error

	gotAtespace     string
	gotImage        string
	gotControlPlane string
}

func (d *providerTestDeps) deps() providerDeps {
	return providerDeps{
		newClient: func(context.Context, ateAPIConnConfig) (ateapi.ControlClient, func() error, error) {
			d.newClientCalls.Add(1)
			if d.newClientErr != nil {
				return nil, nil, d.newClientErr
			}
			return d.client, func() error {
				d.closeCalls.Add(1)
				return d.closeErr
			}, nil
		},
		ensureTemplate: func(_ context.Context, _ ateapi.ControlClient, atespace, image, controlPlane string) (string, error) {
			d.ensureTemplateCalls.Add(1)
			d.gotAtespace, d.gotImage, d.gotControlPlane = atespace, image, controlPlane
			if d.ensureTemplateErr != nil {
				return "", d.ensureTemplateErr
			}
			return d.template, nil
		},
	}
}

func TestNewRejectsInvalidConfigBeforeAnyNetworkCall(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{
			name:      "empty atespace",
			mutate:    func(c *Config) { c.Atespace = "" },
			wantField: "Config.Atespace",
		},
		{
			name:      "image without digest",
			mutate:    func(c *Config) { c.Image = "registry.zhanwei.wang/castworks/providertest:latest" },
			wantField: "Config.Image",
		},
		{
			name:      "image with empty name",
			mutate:    func(c *Config) { c.Image = "@sha256:" + strings.Repeat("a", 64) },
			wantField: "Config.Image",
		},
		{
			name: "image with uppercase digest",
			mutate: func(c *Config) {
				c.Image = "registry.zhanwei.wang/castworks/providertest@sha256:" + strings.Repeat("A", 64)
			},
			wantField: "Config.Image",
		},
		{
			name: "image with short digest",
			mutate: func(c *Config) {
				c.Image = "registry.zhanwei.wang/castworks/providertest@sha256:" + strings.Repeat("a", 63)
			},
			wantField: "Config.Image",
		},
		{
			name: "image with non-hexadecimal digest",
			mutate: func(c *Config) {
				c.Image = "registry.zhanwei.wang/castworks/providertest@sha256:" + strings.Repeat("g", 64)
			},
			wantField: "Config.Image",
		},
		{
			name:      "control plane without port",
			mutate:    func(c *Config) { c.ControlPlane = "castworks-dev.castworks-system.svc.cluster.local" },
			wantField: "Config.ControlPlane",
		},
		{
			name:      "control plane with empty host",
			mutate:    func(c *Config) { c.ControlPlane = ":9090" },
			wantField: "Config.ControlPlane",
		},
		{
			name:      "control plane with non-numeric port",
			mutate:    func(c *Config) { c.ControlPlane = "castworks-dev.castworks-system.svc.cluster.local:grpc" },
			wantField: "Config.ControlPlane",
		},
		{
			name:      "control plane with port zero",
			mutate:    func(c *Config) { c.ControlPlane = "castworks-dev.castworks-system.svc.cluster.local:0" },
			wantField: "Config.ControlPlane",
		},
		{
			name:      "control plane with port above 65535",
			mutate:    func(c *Config) { c.ControlPlane = "castworks-dev.castworks-system.svc.cluster.local:70000" },
			wantField: "Config.ControlPlane",
		},
		{
			name:      "service account without namespace",
			mutate:    func(c *Config) { c.ServiceAccount = "sandboxprovider" },
			wantField: "Config.ServiceAccount",
		},
		{
			name:      "service account with empty namespace",
			mutate:    func(c *Config) { c.ServiceAccount = "/sandboxprovider" },
			wantField: "Config.ServiceAccount",
		},
		{
			name:      "service account with empty name",
			mutate:    func(c *Config) { c.ServiceAccount = "castworks-system/" },
			wantField: "Config.ServiceAccount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := providerTestConfig()
			tt.mutate(&cfg)
			deps := &providerTestDeps{client: &fakeControlClient{}, template: providerTestTemplate}

			p, err := newProvider(context.Background(), cfg, deps.deps())
			if err == nil {
				t.Fatalf("newProvider() = %v, want an error", p)
			}
			if !strings.Contains(err.Error(), tt.wantField) {
				t.Errorf("error %q does not name %s", err, tt.wantField)
			}
			if got := deps.newClientCalls.Load(); got != 0 {
				t.Errorf("newClient called %d times for an invalid Config, want 0", got)
			}
			if got := deps.ensureTemplateCalls.Load(); got != 0 {
				t.Errorf("ensureTemplate called %d times for an invalid Config, want 0", got)
			}
		})
	}
}

func TestNewWiresProviderAndManager(t *testing.T) {
	client := &fakeControlClient{
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return &ateapi.Actor{Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING}}, nil
		},
	}
	deps := &providerTestDeps{client: client, template: providerTestTemplate}

	cfg := providerTestConfig()
	p, err := newProvider(context.Background(), cfg, deps.deps())
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}

	if p.manager.client != client {
		t.Errorf("manager client = %v, want the client from newClient", p.manager.client)
	}
	if p.manager.atespace != cfg.Atespace {
		t.Errorf("manager atespace = %q, want %q", p.manager.atespace, cfg.Atespace)
	}
	if p.manager.template != providerTestTemplate {
		t.Errorf("manager template = %q, want %q", p.manager.template, providerTestTemplate)
	}
	if p.manager.controlPlane != cfg.ControlPlane {
		t.Errorf("manager controlPlane = %q, want %q", p.manager.controlPlane, cfg.ControlPlane)
	}
	if deps.gotAtespace != cfg.Atespace || deps.gotImage != cfg.Image || deps.gotControlPlane != cfg.ControlPlane {
		t.Errorf("ensureTemplate got (%q, %q, %q), want (%q, %q, %q)",
			deps.gotAtespace, deps.gotImage, deps.gotControlPlane, cfg.Atespace, cfg.Image, cfg.ControlPlane)
	}

	status, err := p.GetSandbox(context.Background(), "cw-run0")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if status.State != sandboxprovider.ActorStateRunning {
		t.Errorf("GetSandbox state = %q, want %q", status.State, sandboxprovider.ActorStateRunning)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := deps.closeCalls.Load(); got != 1 {
		t.Errorf("close called %d times, want 1", got)
	}
}

func TestProviderDelegatesLifecycleMethods(t *testing.T) {
	client := &fakeControlClient{
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return &ateapi.Actor{Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}}, nil
		},
	}
	deps := &providerTestDeps{client: client, template: providerTestTemplate}
	p, err := newProvider(context.Background(), providerTestConfig(), deps.deps())
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	if _, err := p.Provision(ctx, "run1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := p.Snapshot(ctx, "cw-run1"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := p.RestoreSnapshot(ctx, "cw-run1"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	status, err := p.GetSandbox(ctx, "cw-run1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if status.State != sandboxprovider.ActorStateSuspended {
		t.Errorf("GetSandbox state = %q, want %q", status.State, sandboxprovider.ActorStateSuspended)
	}
	if err := p.Destroy(ctx, "cw-run1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []string{
		"CreateActor", "CreateActorEgressPolicy", "ResumeActor",
		"SuspendActor", "ResumeActor", "GetActor", "DeleteActor",
	}
	got := client.callList()
	if len(got) != len(want) {
		t.Fatalf("RPC calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RPC calls = %v, want %v", got, want)
		}
	}
}

func TestNewClosesConnectionWhenTemplateSetupFails(t *testing.T) {
	sentinel := errors.New("template setup failed")
	deps := &providerTestDeps{
		client:            &fakeControlClient{},
		ensureTemplateErr: sentinel,
	}

	p, err := newProvider(context.Background(), providerTestConfig(), deps.deps())
	if p != nil {
		t.Errorf("newProvider returned a Provider on failure: %v", p)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("newProvider error = %v, want %v", err, sentinel)
	}
	if got := deps.newClientCalls.Load(); got != 1 {
		t.Errorf("newClient called %d times, want 1", got)
	}
	if got := deps.closeCalls.Load(); got != 1 {
		t.Errorf("connection closed %d times after a failed template step, want 1", got)
	}
}

func TestNewReturnsConnectionErrorWithoutTouchingTemplates(t *testing.T) {
	sentinel := errors.New("cannot reach ate-api")
	deps := &providerTestDeps{newClientErr: sentinel}

	p, err := newProvider(context.Background(), providerTestConfig(), deps.deps())
	if p != nil {
		t.Errorf("newProvider returned a Provider on failure: %v", p)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("newProvider error = %v, want %v", err, sentinel)
	}
	if got := deps.ensureTemplateCalls.Load(); got != 0 {
		t.Errorf("ensureTemplate called %d times after a failed connection, want 0", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		deps := &providerTestDeps{client: &fakeControlClient{}, template: providerTestTemplate}
		p, err := newProvider(context.Background(), providerTestConfig(), deps.deps())
		if err != nil {
			t.Fatalf("newProvider: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := p.Close(); err != nil {
				t.Fatalf("Close call %d: %v", i+1, err)
			}
		}
		if got := deps.closeCalls.Load(); got != 1 {
			t.Errorf("close called %d times, want 1", got)
		}
	})

	t.Run("returns the first error again", func(t *testing.T) {
		sentinel := errors.New("close failed")
		deps := &providerTestDeps{client: &fakeControlClient{}, template: providerTestTemplate, closeErr: sentinel}
		p, err := newProvider(context.Background(), providerTestConfig(), deps.deps())
		if err != nil {
			t.Fatalf("newProvider: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := p.Close(); !errors.Is(err, sentinel) {
				t.Fatalf("Close call %d = %v, want %v", i+1, err, sentinel)
			}
		}
		if got := deps.closeCalls.Load(); got != 1 {
			t.Errorf("close called %d times, want 1", got)
		}
	})
}
