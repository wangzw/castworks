// Package substrate implements sandboxprovider.Provider on Substrate v1.0.0.
//
// It covers docs/milestone/v0.1.0/sandboxprovider.md's "职责", "接口",
// "actor 模板", "网络", "调用 ate-api" and "错误处理" sections, with the
// v0.2.0 changes in docs/milestone/v0.2.0/sandboxprovider.md: it keeps the
// actor template for the configured image and addresses, and creates,
// suspends, resumes and deletes one actor per run through ate-api.
//
// The connection code in conn.go, the template creation in template.go and
// the RPC sequence in actor.go are adapted from Substrate v1.0.0 under the
// Apache License, Version 2.0; each file header names its source files.
package substrate

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"go.ate.dev/substrate/api/ateapi/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

// Config configures a Provider. The zero Config is invalid; New checks every
// field before it opens a connection.
type Config struct {
	Kubeconfig     string   // 空则用默认加载规则
	KubeContext    string   // 空则用当前 context
	Atespace       string   // castworks-dev
	Image          string   // name@sha256:...
	ControlPlane   string   // 写进模板 env 的 CASTWORKS_CONTROL_PLANE，host:port
	Connector      string   // 写进模板 env 的 CASTWORKS_CONNECTOR，host:port
	EgressHTTP     []string // 额外的明文 HTTP 出口，host:port，主机必须是全限定名
	ServiceAccount string   // castworks-system/sandboxprovider
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
	ensureTemplate func(ctx context.Context, client ateapi.ControlClient, atespace, image, controlPlane, connector string) (string, error)
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

	template, err := deps.ensureTemplate(ctx, client, cfg.Atespace, cfg.Image, cfg.ControlPlane, cfg.Connector)
	if err != nil {
		if closeErr := closeClient(); closeErr != nil {
			slog.Default().WarnContext(ctx, "close ate-api connection after template setup failed",
				"error", closeErr)
		}
		return nil, fmt.Errorf("substrate: ensure actor template: %w", err)
	}

	return &Provider{
		client:  client,
		manager: newActorManager(client, cfg.Atespace, template, cfg.ControlPlane, cfg.Connector, cfg.EgressHTTP),
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
	if err := validateEndpoint("Config.ControlPlane", cfg.ControlPlane); err != nil {
		return err
	}
	if err := validateEndpoint("Config.Connector", cfg.Connector); err != nil {
		return err
	}
	if err := validateEgressHTTP(cfg.EgressHTTP); err != nil {
		return err
	}
	if _, _, err := splitServiceAccount(cfg.ServiceAccount); err != nil {
		return fmt.Errorf("substrate: Config.ServiceAccount: %w", err)
	}
	return nil
}

// validateImage requires <name>@sha256:<64 lowercase hexadecimal digits>.
func validateImage(image string) error {
	const marker = "@sha256:"
	i := strings.LastIndex(image, marker)
	if i <= 0 {
		return fmt.Errorf("substrate: Config.Image %q is not of the form <name>%s<64 lowercase hexadecimal digits>", image, marker)
	}
	digest := image[i+len(marker):]
	if len(digest) != 64 {
		return fmt.Errorf("substrate: Config.Image %q has a sha256 digest of %d hexadecimal digits, want 64", image, len(digest))
	}
	if digest != strings.ToLower(digest) {
		return fmt.Errorf("substrate: Config.Image %q has an uppercase sha256 digest", image)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("substrate: Config.Image %q has a malformed sha256 digest: %w", image, err)
	}
	return nil
}

// validateEndpoint requires field to be host:port with a fully qualified host
// name and a numeric port. It validates the control-plane and connector
// addresses; validateEgressHTTP applies the same validateFullyQualifiedHost to
// every EgressHTTP entry, so all three fields accept exactly the same hosts.
// They must, because all of them become egress rules that match the request
// authority.
func validateEndpoint(field, address string) error {
	host, _, err := splitHostPort(field, address)
	if err != nil {
		return err
	}
	return validateFullyQualifiedHost(field, host)
}

// splitHostPort parses host:port and checks that the host is non-empty and the
// port is a number from 1 to 65535. Both the configuration checks and the
// policy builder use it so that they accept exactly the same addresses.
func splitHostPort(field, address string) (string, int32, error) {
	if address == "" {
		return "", 0, fmt.Errorf("substrate: %s is empty", field)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("substrate: %s %q is not host:port: %w", field, address, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("substrate: %s %q has an empty host", field, address)
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("substrate: %s %q has port %q, want a number from 1 to 65535", field, address, portText)
	}
	return host, int32(port), nil
}

// validateEgressHTTP checks every Config.EgressHTTP entry: host:port with a
// fully qualified host name and no repeated entry. A repeated entry is
// rejected because it almost certainly means the same address was listed
// twice, while the same host with different ports is meaningful and allowed.
func validateEgressHTTP(entries []string) error {
	seen := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		field := fmt.Sprintf("Config.EgressHTTP[%d]", i)
		host, port, err := splitHostPort(field, entry)
		if err != nil {
			return err
		}
		if err := validateFullyQualifiedHost(field, host); err != nil {
			return err
		}
		// Compare the parsed pair, so 9100 and 09100 count as the same port.
		key := fmt.Sprintf("%s:%d", host, port)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("substrate: %s %q is repeated", field, entry)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateFullyQualifiedHost requires host to be a fully qualified domain
// name. The egress policy matches the request authority and the actor's DNS
// relay only forwards names listed in the policy, so a short name would depend
// on search-domain completion; an IP literal has no name to match at all.
// A name ending in ".svc" is still a short name: the DNS search domain
// completes it to "<name>.svc.cluster.local", which is not what the policy
// matches.
func validateFullyQualifiedHost(field, host string) error {
	if net.ParseIP(host) != nil {
		return fmt.Errorf("substrate: %s host %q is an IP address, want a fully qualified domain name", field, host)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("substrate: %s host %q is not a fully qualified domain name", field, host)
	}
	if strings.HasSuffix(strings.ToLower(host), ".svc") {
		return fmt.Errorf("substrate: %s host %q ends in .svc and relies on search-domain completion, want a fully qualified domain name", field, host)
	}
	if errs := validation.IsDNS1123Subdomain(host); len(errs) > 0 {
		return fmt.Errorf("substrate: %s host %q is not a valid fully qualified domain name: %s", field, host, strings.Join(errs, "; "))
	}
	return nil
}
