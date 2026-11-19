// Package substrate implements sandboxprovider.Provider on Substrate v1.0.0.
//
// It covers docs/milestone/v0.1.0/sandboxprovider.md's "职责", "接口",
// "actor 模板", "网络", "调用 ate-api" and "错误处理" sections: it keeps the
// actor template for the configured image and creates, suspends, resumes and
// deletes one actor per run through ate-api.
//
// The connection code in conn.go, the template creation in template.go and
// the RPC sequence in actor.go are adapted from Substrate v1.0.0 under the
// Apache License, Version 2.0; each file header names its source files.
package substrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"go.ate.dev/substrate/api/ateapi/v1"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

// Config configures a Provider. The zero Config is invalid; New checks every
// field before it opens a connection.
type Config struct {
	Kubeconfig     string // 空则用默认加载规则
	KubeContext    string // 空则用当前 context
	Atespace       string // castworks-dev
	Image          string // name@sha256:...
	ControlPlane   string // 写进模板 env 的 CASTWORKS_CONTROL_PLANE，host:port
	ServiceAccount string // castworks-system/sandboxprovider
}

// Provider is a sandboxprovider.Provider backed by ate-api. It is safe for
// concurrent calls on different actors.
type Provider struct {
	client  ateapi.ControlClient
	manager *actorManager
	close   func() error

	closeOnce sync.Once
	closeErr  error
}

var _ sandboxprovider.Provider = (*Provider)(nil)

// providerDeps holds the seams the constructor uses. Production code uses
// defaultProviderDeps; unit tests replace the connection and template steps so
// that no cluster is needed.
type providerDeps struct {
	newClient      func(ctx context.Context, cfg ateAPIConnConfig) (ateapi.ControlClient, func() error, error)
	ensureTemplate func(ctx context.Context, client ateapi.ControlClient, atespace, image, controlPlane string) (string, error)
}

var defaultProviderDeps = providerDeps{
	newClient:      newAteAPIClient,
	ensureTemplate: ensureActorTemplate,
}

// New connects to ate-api and ensures the actor template for cfg.Image exists.
// ctx bounds only this initialization: the background port-forward rebuilds
// and token renewals of the connection keep running until Close. A failure in
// any step releases the connection opened so far.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	return newProvider(ctx, cfg, defaultProviderDeps)
}

func newProvider(ctx context.Context, cfg Config, deps providerDeps) (*Provider, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	client, closeClient, err := deps.newClient(ctx, ateAPIConnConfig{
		Kubeconfig:     cfg.Kubeconfig,
		KubeContext:    cfg.KubeContext,
		ServiceAccount: cfg.ServiceAccount,
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: connect to ate-api: %w", err)
	}

	template, err := deps.ensureTemplate(ctx, client, cfg.Atespace, cfg.Image, cfg.ControlPlane)
	if err != nil {
		if closeErr := closeClient(); closeErr != nil {
			slog.Default().WarnContext(ctx, "close ate-api connection after template setup failed",
				"error", closeErr)
		}
		return nil, fmt.Errorf("substrate: ensure actor template: %w", err)
	}

	return &Provider{
		client:  client,
		manager: newActorManager(client, cfg.Atespace, template, cfg.ControlPlane),
		close:   closeClient,
	}, nil
}

// Close stops the background token renewal and port-forward and releases the
// connection. It is idempotent and returns the same error on every call.
func (p *Provider) Close() error {
	p.closeOnce.Do(func() {
		if p.close != nil {
			p.closeErr = p.close()
		}
	})
	return p.closeErr
}

// Provision implements sandboxprovider.Provider.
func (p *Provider) Provision(ctx context.Context, runID string) (sandboxprovider.Timing, error) {
	return p.manager.Provision(ctx, runID)
}

// Snapshot implements sandboxprovider.Provider.
func (p *Provider) Snapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	return p.manager.Snapshot(ctx, actor)
}

// RestoreSnapshot implements sandboxprovider.Provider.
func (p *Provider) RestoreSnapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	return p.manager.RestoreSnapshot(ctx, actor)
}

// Destroy implements sandboxprovider.Provider.
func (p *Provider) Destroy(ctx context.Context, actor string) error {
	return p.manager.Destroy(ctx, actor)
}

// GetSandbox implements sandboxprovider.Provider.
func (p *Provider) GetSandbox(ctx context.Context, actor string) (sandboxprovider.SandboxStatus, error) {
	return p.manager.GetSandbox(ctx, actor)
}

// validateConfig checks every field without touching the network.
func validateConfig(cfg Config) error {
	if cfg.Atespace == "" {
		return errors.New("substrate: Config.Atespace is empty")
	}
	if err := validateImage(cfg.Image); err != nil {
		return err
	}
	if err := validateControlPlane(cfg.ControlPlane); err != nil {
		return err
	}
	if _, _, err := splitServiceAccount(cfg.ServiceAccount); err != nil {
		return fmt.Errorf("substrate: Config.ServiceAccount: %w", err)
	}
	return nil
}

// validateImage requires <name>@sha256:<64 lowercase hexadecimal digits>.
// actorTemplateName owns the digest rules; this function adds the requirement
// that the image name is not empty and names the Config field in the error.
func validateImage(image string) error {
	const marker = "@sha256:"
	if i := strings.LastIndex(image, marker); i <= 0 {
		return fmt.Errorf("substrate: Config.Image %q is not of the form <name>%s<64 lowercase hexadecimal digits>", image, marker)
	}
	if _, err := actorTemplateName(image); err != nil {
		return fmt.Errorf("substrate: Config.Image %q: %w", image, err)
	}
	return nil
}

// validateControlPlane requires host:port with a numeric port.
func validateControlPlane(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("substrate: Config.ControlPlane %q is not host:port: %w", address, err)
	}
	if host == "" {
		return fmt.Errorf("substrate: Config.ControlPlane %q has an empty host", address)
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("substrate: Config.ControlPlane %q has port %q, want a number from 1 to 65535", address, portText)
	}
	return nil
}
