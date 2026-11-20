package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

const (
	configTestDatabaseURL = "postgres://castworks:castworks@127.0.0.1:55432/castworks?sslmode=disable"
	configTestImage       = "actor@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// configRequiredArgs 返回除 --database-url 外的必填参数。
func configRequiredArgs(provider string) []string {
	return []string{
		"--llm-credential-file", "/tmp/castworks-controlplane/test-token",
		"--llm-credential-kind", "oauth_token",
		"--provider", provider,
	}
}

// configBaseArgs 返回一组能通过校验的参数；provider 为 substrate 时不含 --substrate-image。
// substrate 要求 gRPC 地址不是回环，所以给一个开发机网卡地址，默认值 127.0.0.1:9090 会被拒。
func configBaseArgs(provider string) []string {
	args := append([]string{"--database-url", configTestDatabaseURL}, configRequiredArgs(provider)...)
	if provider == ProviderSubstrate {
		args = append(args, "--grpc-addr", "10.12.72.11:9090")
	}
	return args
}

func configParse(args []string, getenv func(string) string) (*Config, error) {
	return parseConfigWithOutput(args, getenv, io.Discard)
}

func configMustParse(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, err := configParse(args, nil)
	if err != nil {
		t.Fatalf("parseConfigWithOutput(%q) failed: %v", args, err)
	}
	return cfg
}

func configWantError(t *testing.T, args []string, wantSubstring string) {
	t.Helper()
	cfg, err := configParse(args, nil)
	if err == nil {
		t.Fatalf("parseConfigWithOutput(%q) = %+v, want error containing %q", args, cfg, wantSubstring)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("parseConfigWithOutput(%q) error = %q, want it to contain %q", args, err, wantSubstring)
	}
}

func withoutFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestConfigDefaults(t *testing.T) {
	cfg := configMustParse(t, configBaseArgs(ProviderLocal)...)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"HTTPAddr", cfg.HTTPAddr, defaultHTTPAddr},
		{"GRPCAddr", cfg.GRPCAddr, defaultGRPCAddr},
		{"DatabaseURL", cfg.DatabaseURL, configTestDatabaseURL},
		{"LLMCredentialFile", cfg.LLMCredentialFile, "/tmp/castworks-controlplane/test-token"},
		{"LLMCredentialKind", cfg.LLMCredentialKind, runcontroller.LLMCredentialOAuthToken},
		{"Provider", cfg.Provider, ProviderLocal},
		{"Suspend", cfg.Suspend, false},
		{"IdleGrace", cfg.IdleGrace, 30 * time.Second},
		{"ScanPeriod", cfg.ScanPeriod, 5 * time.Second},
		{"HelloTimeout", cfg.HelloTimeout, 60 * time.Second},
		{"ReconnectTimeout", cfg.ReconnectTimeout, 30 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 10 * time.Second},
		{"SubstrateKubeconfig", cfg.SubstrateKubeconfig, ""},
		{"SubstrateContext", cfg.SubstrateContext, ""},
		{"SubstrateAtespace", cfg.SubstrateAtespace, "castworks-dev"},
		{"SubstrateImage", cfg.SubstrateImage, ""},
		{"SubstrateControlPlane", cfg.SubstrateControlPlane, "castworks-dev.castworks-system.svc.cluster.local:9090"},
		{"SubstrateConnector", cfg.SubstrateConnector, "castworks-dev.castworks-system.svc.cluster.local:9091"},
		{"SubstrateServiceAccount", cfg.SubstrateServiceAccount, "castworks-system/sandboxprovider"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.SubstrateEgressHTTP) != 0 {
		t.Errorf("SubstrateEgressHTTP = %v, want empty", cfg.SubstrateEgressHTTP)
	}
}

func TestConfigDatabaseURLFromEnv(t *testing.T) {
	getenv := func(key string) string {
		if key == envDatabaseURL {
			return configTestDatabaseURL
		}
		return ""
	}
	cfg, err := configParse(configRequiredArgs(ProviderLocal), getenv)
	if err != nil {
		t.Fatalf("parseConfigWithOutput() failed: %v", err)
	}
	if cfg.DatabaseURL != configTestDatabaseURL {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, configTestDatabaseURL)
	}
}

func TestConfigEmptyDatabaseURLFlagFallsBackToEnv(t *testing.T) {
	const envURL = "postgres://env@127.0.0.1:5432/envdb"
	tests := []struct {
		name string
		args []string
	}{
		{"separate empty value", append([]string{"--database-url", ""}, configRequiredArgs(ProviderLocal)...)},
		{"empty equals form", append([]string{"--database-url="}, configRequiredArgs(ProviderLocal)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := configParse(tt.args, func(string) string { return envURL })
			if err != nil {
				t.Fatalf("parseConfigWithOutput() failed: %v", err)
			}
			if cfg.DatabaseURL != envURL {
				t.Errorf("DatabaseURL = %q, want the environment value %q", cfg.DatabaseURL, envURL)
			}
		})
	}
}

func TestConfigRejectsPositionalArgs(t *testing.T) {
	const positional = "surprise-positional-value"
	args := append(configBaseArgs(ProviderLocal), positional, "second-extra")

	_, err := configParse(args, nil)
	if err == nil {
		t.Fatal("parseConfigWithOutput() succeeded, want an error for positional arguments")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error = %q, want it to report the count 2", err)
	}
	if strings.Contains(err.Error(), positional) || strings.Contains(err.Error(), "second-extra") {
		t.Errorf("error = %q, must not echo the positional argument contents", err)
	}
}

func TestConfigRedactsDatabasePassword(t *testing.T) {
	const (
		userinfoPassword = "sup3r-secret-do-not-leak"
		queryPassword    = "query-param-secret-do-not-leak"
		sslPassword      = "ssl-param-secret-do-not-leak"
		keyValuePassword = "key-value-secret-do-not-leak"
	)
	cases := []struct {
		name    string
		dbURL   string
		secrets []string
	}{
		{
			name:    "userinfo",
			dbURL:   "postgres://castworks:" + userinfoPassword + "@127.0.0.1:55432/castworks",
			secrets: []string{userinfoPassword},
		},
		{
			name:    "query parameters",
			dbURL:   "postgres://castworks@127.0.0.1:55432/castworks?password=" + queryPassword + "&sslpassword=" + sslPassword,
			secrets: []string{queryPassword, sslPassword},
		},
		{
			name:    "key-value form",
			dbURL:   "host=127.0.0.1 port=55432 user=castworks password=" + keyValuePassword + " dbname=castworks",
			secrets: []string{keyValuePassword},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"--database-url", c.dbURL}, configRequiredArgs(ProviderLocal)...)
			cfg := configMustParse(t, args...)

			outputs := []struct {
				name string
				text string
			}{
				{"String (%v)", fmt.Sprintf("%v", cfg)},
				{"String (%+v)", fmt.Sprintf("%+v", cfg)},
				{"GoString (%#v)", fmt.Sprintf("%#v", cfg)},
				{"LogValue", cfg.LogValue().String()},
			}
			for _, out := range outputs {
				for _, secret := range c.secrets {
					if strings.Contains(out.text, secret) {
						t.Errorf("%s leaks the database password %q: %q", out.name, secret, out.text)
					}
				}
				if !strings.Contains(out.text, "xxxxx") {
					t.Errorf("%s = %q, want the password masked", out.name, out.text)
				}
			}
		})
	}
}

func TestConfigDatabaseURLFlagWinsOverEnv(t *testing.T) {
	const flagURL = "postgres://flag@127.0.0.1:5432/flagdb"
	args := append([]string{"--database-url", flagURL}, configRequiredArgs(ProviderLocal)...)
	cfg, err := configParse(args, func(string) string { return "postgres://env@127.0.0.1:5432/envdb" })
	if err != nil {
		t.Fatalf("parseConfigWithOutput() failed: %v", err)
	}
	if cfg.DatabaseURL != flagURL {
		t.Errorf("DatabaseURL = %q, want the flag value %q", cfg.DatabaseURL, flagURL)
	}
}

func TestConfigRequired(t *testing.T) {
	base := configBaseArgs(ProviderLocal)
	cases := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"database-url", withoutFlag(base, "database-url"), "--database-url"},
		{"llm-credential-file", withoutFlag(base, "llm-credential-file"), "--llm-credential-file"},
		{"llm-credential-kind", withoutFlag(base, "llm-credential-kind"), "--llm-credential-kind"},
		{"provider", withoutFlag(base, "provider"), "--provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configWantError(t, c.args, c.wantFlag)
		})
	}
}

func TestConfigInvalidProvider(t *testing.T) {
	args := []string{
		"--database-url", configTestDatabaseURL,
		"--llm-credential-file", "/tmp/token",
		"--llm-credential-kind", "oauth_token",
		"--provider", "docker",
	}
	configWantError(t, args, "--provider")
}

func TestConfigInvalidCredentialKind(t *testing.T) {
	for _, kind := range []string{"bearer", "apikey", "API_KEY"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{
				"--database-url", configTestDatabaseURL,
				"--llm-credential-file", "/tmp/token",
				"--llm-credential-kind", kind,
				"--provider", "local",
			}
			configWantError(t, args, "--llm-credential-kind")
		})
	}
}

func TestConfigHTTPAddr(t *testing.T) {
	accepted := []string{
		"127.0.0.1:8080",
		"127.0.0.1:0",
		"[::1]:8080",
		"[::1]:0",
	}
	rejected := []string{
		"127.0.0.2:8080",
		"0.0.0.0:8080",
		":8080",
		"[::]:8080",
		"10.12.72.11:8080",
		"localhost:8080",
		"example.com:80",
		"127.0.0.1",
		"127.0.0.1:65536",
		"127.0.0.1:-1",
		"127.0.0.1:notaport",
	}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			args := append(configBaseArgs(ProviderLocal), "--http-addr", addr)
			configMustParse(t, args...)
		})
	}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			args := append(configBaseArgs(ProviderLocal), "--http-addr", addr)
			configWantError(t, args, "--http-addr")
		})
	}
}

func TestConfigGRPCAddrSubstrate(t *testing.T) {
	argsFor := func(addr string) []string {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
		return append(args, "--grpc-addr", addr)
	}
	accepted := []string{
		"10.12.72.11:9090",
	}
	rejected := []string{
		"127.0.0.1:9090",
		"[::1]:9090",
		"0.0.0.0:9090",
		"[::]:9090",
		"localhost:9090",
		"example.com:9090",
		"10.12.72.11:0",
		"10.12.72.11",
	}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			configMustParse(t, argsFor(addr)...)
		})
	}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			configWantError(t, argsFor(addr), "--grpc-addr")
		})
	}
}

func TestConfigLocalRejectsSubstrateFlags(t *testing.T) {
	flags := map[string]string{
		"substrate-kubeconfig":      "/tmp/kubeconfig",
		"substrate-context":         "dev",
		"substrate-atespace":        "other",
		"substrate-image":           configTestImage,
		"substrate-control-plane":   "10.12.72.11:9090",
		"substrate-connector":       "castworks-dev.castworks-system.svc.cluster.local:9091",
		"substrate-egress-http":     "extsim.castworks-system.svc.cluster.local:9100",
		"substrate-service-account": "ns/name",
	}
	for name, value := range flags {
		t.Run(name, func(t *testing.T) {
			args := append(configBaseArgs(ProviderLocal), "--"+name, value)
			configWantError(t, args, "--"+name)
		})
	}
}

func TestConfigSubstrateImage(t *testing.T) {
	base := configBaseArgs(ProviderSubstrate)

	t.Run("required", func(t *testing.T) {
		configWantError(t, base, "--substrate-image")
	})

	for _, image := range []string{
		"actor",
		"actor@sha256:",
		"actor@sha256:xyz",
		"actor@sha256:" + strings.Repeat("a", 63),
		"actor@sha256:" + strings.Repeat("a", 65),
		"actor@sha256:" + strings.Repeat("A", 64),
		"actor@sha256:" + strings.Repeat("a", 32) + strings.Repeat("A", 32),
		"actor@sha512:" + strings.Repeat("a", 64),
		"@sha256:" + strings.Repeat("a", 64),
	} {
		t.Run("reject/"+image, func(t *testing.T) {
			args := append(base, "--substrate-image", image)
			configWantError(t, args, "--substrate-image")
		})
	}

	t.Run("accept", func(t *testing.T) {
		args := append(base, "--substrate-image", configTestImage)
		configMustParse(t, args...)
	})
}

func TestConfigSubstrateAtespace(t *testing.T) {
	t.Run("accept non-default", func(t *testing.T) {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage, "--substrate-atespace", "other-dev")
		cfg := configMustParse(t, args...)
		if cfg.SubstrateAtespace != "other-dev" {
			t.Errorf("SubstrateAtespace = %q, want %q", cfg.SubstrateAtespace, "other-dev")
		}
	})

	t.Run("reject empty", func(t *testing.T) {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage, "--substrate-atespace", "")
		configWantError(t, args, "--substrate-atespace")
	})
}

func TestConfigSubstrateControlPlane(t *testing.T) {
	argsFor := func(addr string) []string {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
		return append(args, "--substrate-control-plane", addr)
	}
	for _, addr := range []string{
		"castworks-dev.castworks-system.svc.cluster.local:9090",
		"10.12.72.11:1",
	} {
		t.Run("accept/"+addr, func(t *testing.T) {
			configMustParse(t, argsFor(addr)...)
		})
	}
	for _, addr := range []string{
		"",
		":9090",
		"host",
		"host:0",
		"host:65536",
		"host:notaport",
	} {
		t.Run("reject/"+addr, func(t *testing.T) {
			configWantError(t, argsFor(addr), "--substrate-control-plane")
		})
	}
}

func TestConfigSubstrateConnector(t *testing.T) {
	argsFor := func(addr string) []string {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
		return append(args, "--substrate-connector", addr)
	}
	for _, addr := range []string{
		"castworks-dev.castworks-system.svc.cluster.local:9091",
		"connector.example:1",
	} {
		t.Run("accept/"+addr, func(t *testing.T) {
			cfg := configMustParse(t, argsFor(addr)...)
			if cfg.SubstrateConnector != addr {
				t.Errorf("SubstrateConnector = %q, want %q", cfg.SubstrateConnector, addr)
			}
		})
	}
	for _, addr := range []string{
		"",
		":9091",
		"host",
		"host:0",
		"host:65536",
		"host:notaport",
	} {
		t.Run("reject/"+addr, func(t *testing.T) {
			configWantError(t, argsFor(addr), "--substrate-connector")
		})
	}
}

func TestConfigSubstrateEgressHTTP(t *testing.T) {
	base := func() []string {
		return append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
	}

	t.Run("empty by default", func(t *testing.T) {
		cfg := configMustParse(t, base()...)
		if len(cfg.SubstrateEgressHTTP) != 0 {
			t.Errorf("SubstrateEgressHTTP = %v, want empty", cfg.SubstrateEgressHTTP)
		}
	})

	t.Run("repeated values are collected in order", func(t *testing.T) {
		want := []string{
			"extsim.castworks-system.svc.cluster.local:9100",
			"metrics.castworks-system.svc.cluster.local:9200",
		}
		args := base()
		for _, addr := range want {
			args = append(args, "--substrate-egress-http", addr)
		}
		cfg := configMustParse(t, args...)
		if !slices.Equal(cfg.SubstrateEgressHTTP, want) {
			t.Errorf("SubstrateEgressHTTP = %v, want %v", cfg.SubstrateEgressHTTP, want)
		}
	})

	for _, addr := range []string{
		"",
		":9100",
		"host",
		"host:0",
		"host:65536",
		"host:notaport",
	} {
		t.Run("reject/"+addr, func(t *testing.T) {
			args := append(base(), "--substrate-egress-http", addr)
			configWantError(t, args, "--substrate-egress-http")
		})
	}
}

func TestConfigSubstrateServiceAccount(t *testing.T) {
	argsFor := func(sa string) []string {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
		return append(args, "--substrate-service-account", sa)
	}
	for _, sa := range []string{
		"castworks-system/sandboxprovider",
		"ns/sa",
	} {
		t.Run("accept/"+sa, func(t *testing.T) {
			configMustParse(t, argsFor(sa)...)
		})
	}
	for _, sa := range []string{
		"",
		"name",
		"/name",
		"ns/",
		"a/b/c",
	} {
		t.Run("reject/"+sa, func(t *testing.T) {
			configWantError(t, argsFor(sa), "--substrate-service-account")
		})
	}
}

func TestConfigDurations(t *testing.T) {
	flags := []string{"idle-grace", "scan-period", "hello-timeout", "reconnect-timeout", "shutdown-timeout"}
	for _, name := range flags {
		for _, value := range []string{"0s", "-1s"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				args := append(configBaseArgs(ProviderLocal), "--"+name, value)
				configWantError(t, args, "--"+name)
			})
		}
	}
}

func TestConfigSuspend(t *testing.T) {
	t.Run("local forces off", func(t *testing.T) {
		cfg := configMustParse(t, append(configBaseArgs(ProviderLocal), "--suspend")...)
		rc, forced := cfg.RunControllerConfig()
		if rc.SuspendEnabled {
			t.Errorf("SuspendEnabled = true, want false for --provider local")
		}
		if !forced {
			t.Errorf("suspendForcedOff = false, want true when --provider local --suspend")
		}
	})

	t.Run("local without suspend", func(t *testing.T) {
		cfg := configMustParse(t, configBaseArgs(ProviderLocal)...)
		rc, forced := cfg.RunControllerConfig()
		if rc.SuspendEnabled {
			t.Errorf("SuspendEnabled = true, want false")
		}
		if forced {
			t.Errorf("suspendForcedOff = true, want false without --suspend")
		}
	})

	t.Run("substrate honors suspend", func(t *testing.T) {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage, "--suspend")
		cfg := configMustParse(t, args...)
		rc, forced := cfg.RunControllerConfig()
		if !rc.SuspendEnabled {
			t.Errorf("SuspendEnabled = false, want true for --provider substrate --suspend")
		}
		if forced {
			t.Errorf("suspendForcedOff = true, want false for substrate")
		}
	})

	t.Run("substrate without suspend", func(t *testing.T) {
		args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", configTestImage)
		cfg := configMustParse(t, args...)
		rc, forced := cfg.RunControllerConfig()
		if rc.SuspendEnabled {
			t.Errorf("SuspendEnabled = true, want false")
		}
		if forced {
			t.Errorf("suspendForcedOff = true, want false")
		}
	})
}

func TestConfigRunControllerConfig(t *testing.T) {
	args := append(configBaseArgs(ProviderSubstrate),
		"--substrate-image", configTestImage,
		"--idle-grace", "45s",
		"--scan-period", "7s",
		"--hello-timeout", "90s",
		"--reconnect-timeout", "45s",
	)
	cfg := configMustParse(t, args...)
	rc, forced := cfg.RunControllerConfig()
	if forced {
		t.Errorf("suspendForcedOff = true, want false")
	}
	if rc.IdleGrace != 45*time.Second {
		t.Errorf("IdleGrace = %v, want 45s", rc.IdleGrace)
	}
	if rc.ScanPeriod != 7*time.Second {
		t.Errorf("ScanPeriod = %v, want 7s", rc.ScanPeriod)
	}
	if rc.HelloTimeout != 90*time.Second {
		t.Errorf("HelloTimeout = %v, want 90s", rc.HelloTimeout)
	}
	if rc.ReconnectTimeout != 45*time.Second {
		t.Errorf("ReconnectTimeout = %v, want 45s", rc.ReconnectTimeout)
	}
	if rc.Credential.Kind != runcontroller.LLMCredentialOAuthToken {
		t.Errorf("Credential.Kind = %q, want %q", rc.Credential.Kind, runcontroller.LLMCredentialOAuthToken)
	}
	if rc.Credential.Value != "" {
		t.Errorf("Credential.Value = %q, want empty; the value is read from the credential file", rc.Credential.Value)
	}
}

func TestConfigDatabaseURLNotLeaked(t *testing.T) {
	const password = "sup3r-secret-do-not-leak"
	dbURL := "postgres://castworks:" + password + "@127.0.0.1:55432/castworks"
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "invalid provider",
			args: []string{
				"--database-url", dbURL,
				"--llm-credential-file", "/tmp/token",
				"--llm-credential-kind", "oauth_token",
				"--provider", "docker",
			},
		},
		{
			name: "missing credential kind",
			args: []string{
				"--database-url", dbURL,
				"--llm-credential-file", "/tmp/token",
				"--provider", "local",
			},
		},
		{
			name: "invalid http addr",
			args: append(append([]string{"--database-url", dbURL}, configRequiredArgs(ProviderLocal)...), "--http-addr", "0.0.0.0:8080"),
		},
		{
			name: "invalid duration",
			args: append(append([]string{"--database-url", dbURL}, configRequiredArgs(ProviderLocal)...), "--idle-grace", "0s"),
		},
		{
			name: "substrate missing image",
			args: append(append([]string{"--database-url", dbURL}, configRequiredArgs(ProviderSubstrate)...), "--grpc-addr", "10.12.72.11:9090"),
		},
		{
			name: "local with substrate flag",
			args: append(append([]string{"--database-url", dbURL}, configRequiredArgs(ProviderLocal)...), "--substrate-atespace", "dev"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := configParse(c.args, nil)
			if err == nil {
				t.Fatalf("parseConfigWithOutput() succeeded, want an error")
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("error text leaks the database password: %q", err)
			}
		})
	}
}

// configConnectorHubArgs returns a valid command line for provider with the
// extra connectorhub flags appended.
func configConnectorHubArgs(provider string, extra ...string) []string {
	base := configBaseArgs(provider)
	if provider == ProviderSubstrate {
		base = append(base, "--substrate-image", configTestImage)
	}
	return append(base, extra...)
}

func TestConfigConnectorHubDefaults(t *testing.T) {
	cfg := configMustParse(t, configBaseArgs(ProviderLocal)...)
	if cfg.ConnectorHubConfig != "" {
		t.Errorf("ConnectorHubConfig = %q, want empty", cfg.ConnectorHubConfig)
	}
	if cfg.ConnectorAddr != "" {
		t.Errorf("ConnectorAddr = %q, want empty", cfg.ConnectorAddr)
	}
	if cfg.WebhookAddr != defaultWebhookAddr {
		t.Errorf("WebhookAddr = %q, want %q", cfg.WebhookAddr, defaultWebhookAddr)
	}
}

func TestConfigWebhookAddr(t *testing.T) {
	accepted := []string{
		"127.0.0.1:9092",
		"127.0.0.1:0",
		"[::1]:9092",
		"[::1]:0",
	}
	rejected := []string{
		"127.0.0.2:9092",
		"0.0.0.0:9092",
		":9092",
		"[::]:9092",
		"10.12.72.11:9092",
		"localhost:9092",
		"example.com:80",
		"127.0.0.1",
		"127.0.0.1:65536",
		"127.0.0.1:-1",
		"127.0.0.1:notaport",
	}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			cfg := configMustParse(t, append(configBaseArgs(ProviderLocal), "--webhook-addr", addr)...)
			if cfg.WebhookAddr != addr {
				t.Errorf("WebhookAddr = %q, want %q", cfg.WebhookAddr, addr)
			}
		})
	}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			args := append(configBaseArgs(ProviderLocal), "--webhook-addr", addr)
			configWantError(t, args, "--webhook-addr")
		})
	}
}

func TestConfigConnectorAddrRequiredWithConfig(t *testing.T) {
	for _, provider := range []string{ProviderLocal, ProviderSubstrate} {
		t.Run(provider, func(t *testing.T) {
			configWantError(t,
				configConnectorHubArgs(provider, "--connectorhub-config", "/tmp/connectorhub.yaml"),
				"--connector-addr")
		})
	}
}

func TestConfigConnectorAddrSubstrate(t *testing.T) {
	argsFor := func(addr string) []string {
		return configConnectorHubArgs(ProviderSubstrate,
			"--connectorhub-config", "/tmp/connectorhub.yaml",
			"--connector-addr", addr)
	}
	accepted := []string{"10.12.72.11:9091"}
	rejected := []string{
		"127.0.0.1:9091",
		"[::1]:9091",
		"0.0.0.0:9091",
		"[::]:9091",
		"localhost:9091",
		"example.com:9091",
		"10.12.72.11:0",
		"10.12.72.11",
	}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			configMustParse(t, argsFor(addr)...)
		})
	}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			configWantError(t, argsFor(addr), "--connector-addr")
		})
	}
}

// TestConfigConnectorAddrLocal covers the local provider, where the connector
// endpoint only has to be bindable: no advertised-address rule applies.
func TestConfigConnectorAddrLocal(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:0",
		"127.0.0.1:9091",
		"10.12.72.11:9091",
	} {
		t.Run("accept/"+addr, func(t *testing.T) {
			cfg := configMustParse(t, configConnectorHubArgs(ProviderLocal,
				"--connectorhub-config", "/tmp/connectorhub.yaml",
				"--connector-addr", addr)...)
			if cfg.ConnectorAddr != addr {
				t.Errorf("ConnectorAddr = %q, want %q", cfg.ConnectorAddr, addr)
			}
		})
	}
}

// TestConfigConnectorAddrWithoutConfig asserts that --connector-addr alone is
// accepted: the flag only matters when --connectorhub-config is set.
func TestConfigConnectorAddrWithoutConfig(t *testing.T) {
	cfg := configMustParse(t, append(configBaseArgs(ProviderLocal), "--connector-addr", "127.0.0.1:0")...)
	if cfg.ConnectorAddr != "127.0.0.1:0" {
		t.Errorf("ConnectorAddr = %q, want %q", cfg.ConnectorAddr, "127.0.0.1:0")
	}
}

// silenceStderr points os.Stderr at the null device while the test runs, so
// runMain's logger and the flag package's usage text do not clutter the test
// output. It restores the original file on cleanup.
func silenceStderr(t *testing.T) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	old := os.Stderr
	os.Stderr = devNull
	t.Cleanup(func() {
		os.Stderr = old
		_ = devNull.Close()
	})
}

// TestRunMainExitCodes tables the mapping runMain applies: a usage error maps
// to 2, --help to 0 and any other error to 1.
func TestRunMainExitCodes(t *testing.T) {
	oldArgs := os.Args
	oldLogger := slog.Default()
	t.Cleanup(func() {
		os.Args = oldArgs
		slog.SetDefault(oldLogger)
	})
	silenceStderr(t)

	missingCredential := filepath.Join(t.TempDir(), "missing-credential")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"usage error", []string{"--provider", "docker"}, exitUsage},
		{"help", []string{"--help"}, exitOK},
		{"other error", runTestArgs(missingCredential), exitFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.Args = append([]string{"castworks-controlplane"}, c.args...)
			if got := runMain(); got != c.want {
				t.Errorf("runMain() = %d, want %d", got, c.want)
			}
		})
	}
}

// databaseNameFromURL returns the database named in a connection URL.
func databaseNameFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		t.Fatal("the test database URL names no database")
	}
	return name
}

// waitForNoDatabaseConnections waits until the server reports no backend
// connected to dbName. It fails if run left the pool open.
func waitForNoDatabaseConnections(t *testing.T, dbName string) {
	t.Helper()
	adminURL := os.Getenv(pgtest.DatabaseURLEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the test database server: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := conn.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE datname = $1", dbName,
		).Scan(&count); err != nil {
			t.Fatalf("count connections to database %s: %v", dbName, err)
		}
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connection(s) to database %s remain after run returned; the pool was not closed", count, dbName)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunHTTPAddrInUseClosesPool occupies a loopback port, starts run with
// that --http-addr and asserts the listen failure is reported as a --http-addr
// error and the startup connection pool is closed.
func TestRunHTTPAddrInUseClosesPool(t *testing.T) {
	pool := pgtest.NewPool(t)
	databaseURL := pool.Config().ConnString()
	dbName := databaseNameFromURL(t, databaseURL)
	// Close the pool this test opened so the only connections left belong to
	// run; otherwise the connection check could never observe zero.
	pool.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on a free loopback port: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	addr := occupied.Addr().String()

	credentialFile := runTestCredentialFile(t, "port-in-use-token")
	args := runTestArgs(credentialFile)
	args = replaceFlag(t, args, "database-url", databaseURL)
	args = replaceFlag(t, args, "http-addr", addr)

	ready := false
	err = run(context.Background(), args, func(string, string) { ready = true })
	if err == nil {
		t.Fatal("run() error = nil, want a --http-addr listen error")
	}
	if ready {
		t.Fatal("run() called the ready callback, want no listener to serve")
	}
	if !strings.Contains(err.Error(), "--http-addr") {
		t.Errorf("run() error = %q, want it to name --http-addr", err)
	}
	var ue *usageError
	if errors.As(err, &ue) {
		t.Errorf("run() error = %v, want a listen error rather than a usage error", err)
	}

	waitForNoDatabaseConnections(t, dbName)
}
