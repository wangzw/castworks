package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// --provider 的两个取值。
const (
	ProviderLocal     = "local"
	ProviderSubstrate = "substrate"
)

// envDatabaseURL 是 --database-url 的后备环境变量。
const envDatabaseURL = "CASTWORKS_DATABASE_URL"

// 命令行参数的默认值。与计划里的参数表一致，冻结。
const (
	defaultHTTPAddr                = "127.0.0.1:8080"
	defaultGRPCAddr                = "127.0.0.1:9090"
	defaultWebhookAddr             = "127.0.0.1:9092"
	defaultIdleGrace               = 30 * time.Second
	defaultScanPeriod              = 5 * time.Second
	defaultHelloTimeout            = 60 * time.Second
	defaultShutdownTimeout         = 10 * time.Second
	defaultSubstrateAtespace       = "castworks-dev"
	defaultSubstrateControlPlane   = "castworks-dev.castworks-system.svc.cluster.local:9090"
	defaultSubstrateConnector      = "castworks-dev.castworks-system.svc.cluster.local:9091"
	defaultSubstrateServiceAccount = "castworks-system/sandboxprovider"
)

// Config 是控制面进程的命令行配置。它只承载参数值，解析与校验之外不产生副作用：
// 不读文件、不连数据库、不监听端口。
type Config struct {
	HTTPAddr                string
	GRPCAddr                string
	ConnectorHubConfig      string
	ConnectorAddr           string
	WebhookAddr             string
	DatabaseURL             string
	LLMCredentialFile       string
	LLMCredentialKind       runcontroller.LLMCredentialKind
	Provider                string
	Suspend                 bool
	IdleGrace               time.Duration
	ScanPeriod              time.Duration
	HelloTimeout            time.Duration
	ShutdownTimeout         time.Duration
	SubstrateKubeconfig     string
	SubstrateContext        string
	SubstrateAtespace       string
	SubstrateImage          string
	SubstrateControlPlane   string
	SubstrateConnector      string
	SubstrateEgressHTTP     []string
	SubstrateServiceAccount string
}

// configPrintView drops Config's formatting methods so fmt can print the
// fields themselves instead of calling back into String or GoString.
type configPrintView Config

// keyValueSecretPattern matches a password keyword in the key-value DSN form,
// such as "password=secret" or "sslpassword='quoted secret'", including the
// whitespace or start of string before it. Only the value is replaced, so the
// rest of the DSN keeps its shape.
var keyValueSecretPattern = regexp.MustCompile(`(?i)(^|\s)(password|sslpassword)\s*=\s*(?:'(?:[^'\\]|\\.)*'|[^\s]*)`)

// redactedDatabaseURL returns the database URL with its passwords replaced.
// It covers the URL form (userinfo plus the password and sslpassword query
// parameters) and the key-value DSN form. A URL that does not parse is
// replaced as a whole, because there is no reliable way to locate the password
// inside it.
func redactedDatabaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "://") {
			return "<invalid database URL>"
		}
		return keyValueSecretPattern.ReplaceAllString(raw, "${1}${2}=xxxxx")
	}
	if u.Scheme == "" {
		return keyValueSecretPattern.ReplaceAllString(raw, "${1}${2}=xxxxx")
	}

	redacted := false
	if u.User != nil {
		username := u.User.Username()
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(username, "xxxxx")
			redacted = true
		}
	}
	queryRedacted := false
	q := u.Query()
	for _, key := range []string{"password", "sslpassword"} {
		if _, ok := q[key]; ok {
			q.Set(key, "xxxxx")
			queryRedacted = true
		}
	}
	if queryRedacted {
		u.RawQuery = q.Encode()
		redacted = true
	}
	if !redacted {
		return raw
	}
	return u.String()
}

// redactedString renders every field with the database password masked.
func (c Config) redactedString() string {
	view := configPrintView(c)
	view.DatabaseURL = redactedDatabaseURL(c.DatabaseURL)
	return fmt.Sprintf("Config%+v", view)
}

// String implements fmt.Stringer so %v and %+v never show the database
// password.
func (c Config) String() string {
	return c.redactedString()
}

// GoString implements fmt.GoStringer so %#v never shows the database password.
func (c Config) GoString() string {
	view := configPrintView(c)
	view.DatabaseURL = redactedDatabaseURL(c.DatabaseURL)
	return fmt.Sprintf("main.Config%+v", view)
}

// LogValue implements slog.LogValuer so a Config logged as an attribute never
// shows the database password.
func (c Config) LogValue() slog.Value {
	return slog.StringValue(c.redactedString())
}

// ParseConfig 解析 args（不含程序名）并校验，返回配置。getenv 用于取
// --database-url 的后备值，可为 nil，此时视为环境变量为空。解析与校验的
// 错误文本指出参数名与违反的规则，且永不含数据库 URL。
func ParseConfig(args []string, getenv func(string) string) (*Config, error) {
	return parseConfigWithOutput(args, getenv, os.Stderr)
}

// parseConfigWithOutput 是 ParseConfig 的实现，output 接收 flag 包的用法与解析错误。
// 测试传入 io.Discard，避免污染测试输出。
func parseConfigWithOutput(args []string, getenv func(string) string, output io.Writer) (*Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	cfg := &Config{}
	var (
		credentialKind string
		provider       string
		databaseURL    string
	)

	fs := flag.NewFlagSet("castworks-controlplane", flag.ContinueOnError)
	fs.SetOutput(output)

	fs.StringVar(&cfg.HTTPAddr, "http-addr", defaultHTTPAddr, "HTTP API 监听地址，只接受 127.0.0.1 或 ::1")
	fs.StringVar(&cfg.GRPCAddr, "grpc-addr", defaultGRPCAddr, "gRPC 监听地址；--provider substrate 时必须是网卡 IP 字面量，不能是回环或通配地址")
	fs.StringVar(&cfg.ConnectorHubConfig, "connectorhub-config", "", "connectorhub 配置文件路径；不给出时不启动 connectorhub，不监听 connector 与 webhook 地址")
	fs.StringVar(&cfg.ConnectorAddr, "connector-addr", "", "connector 端点监听地址；给出 --connectorhub-config 时必填，--provider substrate 时规则同 --grpc-addr")
	fs.StringVar(&cfg.WebhookAddr, "webhook-addr", defaultWebhookAddr, "webhook 接收端监听地址，只接受 127.0.0.1 或 ::1")
	fs.StringVar(&databaseURL, "database-url", "", "数据库连接 URL；未给出时取环境变量 "+envDatabaseURL)
	fs.StringVar(&cfg.LLMCredentialFile, "llm-credential-file", "", "LLM 凭据文件路径（必填）")
	fs.StringVar(&credentialKind, "llm-credential-kind", "", "LLM 凭据种类：api_key 或 oauth_token（必填）")
	fs.StringVar(&provider, "provider", "", "沙箱提供者：substrate 或 local（必填）")
	fs.BoolVar(&cfg.Suspend, "suspend", false, "打开挂起扫描；--provider local 时强制关闭")
	fs.DurationVar(&cfg.IdleGrace, "idle-grace", defaultIdleGrace, "空闲多久后挂起（G_idle）")
	fs.DurationVar(&cfg.ScanPeriod, "scan-period", defaultScanPeriod, "挂起扫描周期（P_scan）")
	fs.DurationVar(&cfg.HelloTimeout, "hello-timeout", defaultHelloTimeout, "等待 Hello 的超时（T_hello）")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", defaultShutdownTimeout, "优雅退出的上限")
	fs.StringVar(&cfg.SubstrateKubeconfig, "substrate-kubeconfig", "", "kubeconfig 路径，空则用默认加载规则")
	fs.StringVar(&cfg.SubstrateContext, "substrate-context", "", "kubeconfig context，空则用当前 context")
	fs.StringVar(&cfg.SubstrateAtespace, "substrate-atespace", defaultSubstrateAtespace, "Substrate 的 atespace 名")
	fs.StringVar(&cfg.SubstrateImage, "substrate-image", "", "actor 镜像的 linux/arm64 清单 digest，格式 name@sha256:<64 位十六进制>；--provider substrate 时必填")
	fs.StringVar(&cfg.SubstrateControlPlane, "substrate-control-plane", defaultSubstrateControlPlane, "写进 actor 模板的 CASTWORKS_CONTROL_PLANE，格式 host:port")
	fs.StringVar(&cfg.SubstrateConnector, "substrate-connector", defaultSubstrateConnector, "写进 actor 模板的 CASTWORKS_CONNECTOR，格式 host:port")
	fs.Func("substrate-egress-http", "额外的明文 HTTP 出口，格式 host:port，可重复给出", func(value string) error {
		cfg.SubstrateEgressHTTP = append(cfg.SubstrateEgressHTTP, value)
		return nil
	})
	fs.StringVar(&cfg.SubstrateServiceAccount, "substrate-service-account", defaultSubstrateServiceAccount, "控制面调用 ate-api 时用 TokenRequest 签发令牌的身份，格式 namespace/name")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		// Only the count is reported: positional arguments are free-form and
		// may carry anything the caller typed, so echoing them would put
		// arbitrary text into the error.
		return nil, fmt.Errorf("unexpected positional arguments: got %d", fs.NArg())
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	// Fall back by value, not by presence: an explicit empty --database-url
	// is the same as not giving the flag at all.
	if databaseURL == "" {
		databaseURL = getenv(envDatabaseURL)
	}
	cfg.DatabaseURL = databaseURL
	cfg.LLMCredentialKind = runcontroller.LLMCredentialKind(credentialKind)
	cfg.Provider = provider

	if err := cfg.validate(visited); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RunControllerConfig 把命令行配置转换成 runcontroller.Config。凭据的 Value 不在这里
// 设置：本函数不读文件，调用方读取凭据文件后填入。--provider local 时
// SuspendEnabled 强制为 false，第二个返回值报告是否发生了这种强制，供装配时写日志。
func (c *Config) RunControllerConfig() (cfg runcontroller.Config, suspendForcedOff bool) {
	suspend := c.Suspend
	if c.Provider == ProviderLocal && suspend {
		suspend = false
		suspendForcedOff = true
	}
	return runcontroller.Config{
		IdleGrace:      c.IdleGrace,
		ScanPeriod:     c.ScanPeriod,
		HelloTimeout:   c.HelloTimeout,
		SuspendEnabled: suspend,
		Credential:     runcontroller.LLMCredential{Kind: c.LLMCredentialKind},
	}, suspendForcedOff
}

func (c *Config) validate(visited map[string]bool) error {
	switch c.Provider {
	case ProviderLocal, ProviderSubstrate:
	case "":
		return fmt.Errorf("--provider: required, must be %q or %q", ProviderLocal, ProviderSubstrate)
	default:
		return fmt.Errorf("--provider: must be %q or %q, got %q", ProviderLocal, ProviderSubstrate, c.Provider)
	}

	switch c.LLMCredentialKind {
	case runcontroller.LLMCredentialAPIKey, runcontroller.LLMCredentialOAuthToken:
	case "":
		return fmt.Errorf("--llm-credential-kind: required, must be %q or %q", runcontroller.LLMCredentialAPIKey, runcontroller.LLMCredentialOAuthToken)
	default:
		return fmt.Errorf("--llm-credential-kind: must be %q or %q, got %q", runcontroller.LLMCredentialAPIKey, runcontroller.LLMCredentialOAuthToken, c.LLMCredentialKind)
	}

	if c.LLMCredentialFile == "" {
		return fmt.Errorf("--llm-credential-file: required")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("--database-url: required, set the flag or %s", envDatabaseURL)
	}

	durations := []struct {
		name  string
		value time.Duration
	}{
		{"idle-grace", c.IdleGrace},
		{"scan-period", c.ScanPeriod},
		{"hello-timeout", c.HelloTimeout},
		{"shutdown-timeout", c.ShutdownTimeout},
	}
	for _, d := range durations {
		if d.value <= 0 {
			return fmt.Errorf("--%s: must be positive, got %s", d.name, d.value)
		}
	}

	if err := validateLoopbackAddr(c.HTTPAddr); err != nil {
		return fmt.Errorf("--http-addr: %w", err)
	}

	// The webhook receiver only ever binds loopback, whether or not connectorhub
	// is enabled.
	if err := validateLoopbackAddr(c.WebhookAddr); err != nil {
		return fmt.Errorf("--webhook-addr: %w", err)
	}
	// --connector-addr has no default because the connector endpoint is
	// unreachable without a listener. It is only required when the module is
	// enabled; a stray value on a disabled module is left unused.
	if c.ConnectorHubConfig != "" && c.ConnectorAddr == "" {
		return fmt.Errorf("--connector-addr: required when --connectorhub-config is set")
	}

	if c.Provider == ProviderLocal {
		for _, name := range substrateFlagNames {
			if visited[name] {
				return fmt.Errorf("--%s: must not be set when --provider=%s", name, ProviderLocal)
			}
		}
		return nil
	}

	// --provider substrate
	if err := validateGRPCAddr(c.GRPCAddr); err != nil {
		return fmt.Errorf("--grpc-addr: %w", err)
	}
	// Under Substrate the connector endpoint is reached through a Service by
	// actors, so it must be an advertised network address like --grpc-addr.
	if c.ConnectorHubConfig != "" {
		if err := validateGRPCAddr(c.ConnectorAddr); err != nil {
			return fmt.Errorf("--connector-addr: %w", err)
		}
	}
	if c.SubstrateAtespace == "" {
		return fmt.Errorf("--substrate-atespace: required when --provider=%s", ProviderSubstrate)
	}
	if c.SubstrateImage == "" {
		return fmt.Errorf("--substrate-image: required when --provider=%s", ProviderSubstrate)
	}
	if !imageDigestPattern.MatchString(c.SubstrateImage) {
		return fmt.Errorf("--substrate-image: must have the form name@sha256:<64 lowercase hex digits>, got %q", c.SubstrateImage)
	}
	if err := validateHostPort(c.SubstrateControlPlane); err != nil {
		return fmt.Errorf("--substrate-control-plane: %w", err)
	}
	if err := validateHostPort(c.SubstrateConnector); err != nil {
		return fmt.Errorf("--substrate-connector: %w", err)
	}
	for _, addr := range c.SubstrateEgressHTTP {
		if err := validateHostPort(addr); err != nil {
			return fmt.Errorf("--substrate-egress-http %q: %w", addr, err)
		}
	}
	if err := validateServiceAccount(c.SubstrateServiceAccount); err != nil {
		return fmt.Errorf("--substrate-service-account: %w", err)
	}
	return nil
}

// substrateFlagNames 是 --provider local 时不允许显式给出的参数。
var substrateFlagNames = []string{
	"substrate-kubeconfig",
	"substrate-context",
	"substrate-atespace",
	"substrate-image",
	"substrate-control-plane",
	"substrate-connector",
	"substrate-egress-http",
	"substrate-service-account",
}

var imageDigestPattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// splitHostPort 拆分 host:port，并要求端口是 0 到 65535 的整数。
func splitHostPort(addr string) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid address %q: %w", addr, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q: must be an integer between 0 and 65535", portStr)
	}
	return host, port, nil
}

// validateLoopbackAddr 要求地址是 HTTP 服务端允许绑定的回环 IP 字面量，
// 即 127.0.0.1 或 ::1；127.0.0.2 等其余回环地址一律拒绝。端口可以为 0。
func validateLoopbackAddr(addr string) error {
	host, _, err := splitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q must be an IP literal", host)
	}
	if !ip.Equal(net.IPv4(127, 0, 0, 1)) && !ip.Equal(net.IPv6loopback) {
		return fmt.Errorf("host %q must be 127.0.0.1 or ::1", host)
	}
	return nil
}

// validateGRPCAddr 要求 host 是 IP 字面量，不是回环也不是通配地址，端口是 1 到 65535。
func validateGRPCAddr(addr string) error {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return err
	}
	if port == 0 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q must be an IP literal", host)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("host %q must not be a loopback address", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("host %q must not be the unspecified address", host)
	}
	return nil
}

// validateHostPort 要求 host 非空、端口是 1 到 65535 的整数；主机可以是域名。
// splitHostPort 允许端口 0（由内核分配临时端口），validateHostPort 不接受它：
// 这些地址要写进 actor 模板或出口策略，必须是具体的端口。
func validateHostPort(addr string) error {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if port == 0 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

// validateServiceAccount 要求 namespace/name 形式，两段都非空。
func validateServiceAccount(sa string) error {
	parts := strings.Split(sa, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("must have the form namespace/name, got %q", sa)
	}
	return nil
}
