package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const providerTestImage = "registry.zhanwei.wang/castworks/controlplane@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// providerMissingKubeconfigPath 返回一个位于约定的临时目录下、必定不存在的路径。
// 测试只读取该路径，不创建任何文件。
func providerMissingKubeconfigPath() string {
	return fmt.Sprintf("/tmp/castworks-scratch/controlplane-substrate/missing-kubeconfig-%d", time.Now().UnixNano())
}

// TestSubstrateProviderConfigMapsFlags 从命令行参数一路解析到 substrate.Config，
// 断言八个 --substrate-* 参数逐一对应，重复的 --substrate-egress-http 按给出顺序成为列表。
func TestSubstrateProviderConfigMapsFlags(t *testing.T) {
	const (
		kubeconfig     = "/tmp/castworks-controlplane/kubeconfig"
		kubeContext    = "dev-context"
		atespace       = "atespace-test"
		controlPlane   = "control.example:9443"
		connector      = "connector.example:9091"
		serviceAccount = "namespace-test/service-account-test"
	)
	egressHTTP := []string{
		"extsim.castworks-system.svc.cluster.local:9100",
		"metrics.castworks-system.svc.cluster.local:9200",
	}
	args := append(configBaseArgs(ProviderSubstrate),
		"--substrate-kubeconfig", kubeconfig,
		"--substrate-context", kubeContext,
		"--substrate-atespace", atespace,
		"--substrate-image", providerTestImage,
		"--substrate-control-plane", controlPlane,
		"--substrate-connector", connector,
		"--substrate-egress-http", egressHTTP[0],
		"--substrate-egress-http", egressHTTP[1],
		"--substrate-service-account", serviceAccount,
	)
	cfg := configMustParse(t, args...)

	got := cfg.substrateProviderConfig()
	if got.Kubeconfig != kubeconfig {
		t.Errorf("Kubeconfig = %q, want %q", got.Kubeconfig, kubeconfig)
	}
	if got.KubeContext != kubeContext {
		t.Errorf("KubeContext = %q, want %q", got.KubeContext, kubeContext)
	}
	if got.Atespace != atespace {
		t.Errorf("Atespace = %q, want %q", got.Atespace, atespace)
	}
	if got.Image != providerTestImage {
		t.Errorf("Image = %q, want %q", got.Image, providerTestImage)
	}
	if got.ControlPlane != controlPlane {
		t.Errorf("ControlPlane = %q, want %q", got.ControlPlane, controlPlane)
	}
	if got.Connector != connector {
		t.Errorf("Connector = %q, want %q", got.Connector, connector)
	}
	if !slices.Equal(got.EgressHTTP, egressHTTP) {
		t.Errorf("EgressHTTP = %v, want %v", got.EgressHTTP, egressHTTP)
	}
	if got.ServiceAccount != serviceAccount {
		t.Errorf("ServiceAccount = %q, want %q", got.ServiceAccount, serviceAccount)
	}
}

// TestSubstrateProviderConfigKeepsEmptyKubeconfigAndContext 断言未给出的
// --substrate-kubeconfig 与 --substrate-context 保持空串，不被替换成默认路径。
func TestSubstrateProviderConfigKeepsEmptyKubeconfigAndContext(t *testing.T) {
	args := append(configBaseArgs(ProviderSubstrate), "--substrate-image", providerTestImage)
	cfg := configMustParse(t, args...)

	got := cfg.substrateProviderConfig()
	if got.Kubeconfig != "" {
		t.Errorf("Kubeconfig = %q, want empty", got.Kubeconfig)
	}
	if got.KubeContext != "" {
		t.Errorf("KubeContext = %q, want empty", got.KubeContext)
	}
	if got.Connector != defaultSubstrateConnector {
		t.Errorf("Connector = %q, want the default %q", got.Connector, defaultSubstrateConnector)
	}
	if len(got.EgressHTTP) != 0 {
		t.Errorf("EgressHTTP = %v, want empty", got.EgressHTTP)
	}
}

// TestNewSubstrateProviderMissingKubeconfig 断言 kubeconfig 不可用时构造返回错误，
// 且不返回可用的提供者。构造之外没有任何监听，启动流程据此终止。
func TestNewSubstrateProviderMissingKubeconfig(t *testing.T) {
	cfg := &Config{
		Provider:                ProviderSubstrate,
		SubstrateKubeconfig:     providerMissingKubeconfigPath(),
		SubstrateAtespace:       defaultSubstrateAtespace,
		SubstrateImage:          providerTestImage,
		SubstrateControlPlane:   defaultSubstrateControlPlane,
		SubstrateConnector:      defaultSubstrateConnector,
		SubstrateServiceAccount: defaultSubstrateServiceAccount,
	}

	p, err := newSubstrateProvider(context.Background(), cfg)
	if err == nil {
		t.Fatal("newSubstrateProvider() error = nil, want an error for a missing kubeconfig")
	}
	if p != nil {
		t.Fatalf("newSubstrateProvider() provider = %v, want nil on error", p)
	}
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("newSubstrateProvider() error = %q, want it to mention kubeconfig", err)
	}
}

// TestSubstrateProviderParamNameError 断言构造错误里的 Config 字段名换成参数名，
// 表外的字段与不含字段名的错误保持原样。
func TestSubstrateProviderParamNameError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		want      string
		unchanged bool
	}{
		{
			name: "control plane field",
			err:  errors.New(`substrate: Config.ControlPlane host "castworks-dev.castworks-system.svc" is not a fully qualified domain name`),
			want: "--substrate-control-plane",
		},
		{
			name: "connector field",
			err:  errors.New(`substrate: Config.Connector host "connector.castworks-system.svc" is not a fully qualified domain name`),
			want: "--substrate-connector",
		},
		{
			name: "egress field keeps the index",
			err:  errors.New(`substrate: Config.EgressHTTP[1] host "extsim" is not a fully qualified domain name`),
			want: "--substrate-egress-http[1]",
		},
		{
			name:      "image field is not in the table",
			err:       errors.New(`substrate: Config.Image "image" is not of the form <name>@sha256:<64 lowercase hexadecimal digits>`),
			unchanged: true,
		},
		{
			name:      "error without a field name",
			err:       errors.New("substrate: connect to ate-api: connection refused"),
			unchanged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := substrateParamNameError(tt.err)
			if got == nil {
				t.Fatal("substrateParamNameError() = nil")
			}
			if tt.unchanged {
				if got != tt.err {
					t.Errorf("substrateParamNameError() = %q, want the original error", got)
				}
				return
			}
			if got == tt.err {
				t.Errorf("substrateParamNameError() returned the original error %q, want a rewritten one", got)
			}
			if !strings.Contains(got.Error(), tt.want) {
				t.Errorf("substrateParamNameError() = %q, want it to contain %q", got, tt.want)
			}
			if strings.Contains(got.Error(), "Config.") {
				t.Errorf("substrateParamNameError() = %q, want no Config field name left", got)
			}
		})
	}
}

// TestNewSubstrateProviderErrorNamesFlags 断言构造失败时错误文本含参数名。
// 用例直接构造 Config，绕开命令行解析。
func TestNewSubstrateProviderErrorNamesFlags(t *testing.T) {
	base := func() *Config {
		return &Config{
			Provider:                ProviderSubstrate,
			SubstrateAtespace:       defaultSubstrateAtespace,
			SubstrateImage:          providerTestImage,
			SubstrateControlPlane:   defaultSubstrateControlPlane,
			SubstrateConnector:      defaultSubstrateConnector,
			SubstrateServiceAccount: defaultSubstrateServiceAccount,
		}
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "control plane",
			mutate: func(cfg *Config) { cfg.SubstrateControlPlane = "control.example:0" },
			want:   "--substrate-control-plane",
		},
		{
			name:   "connector short name",
			mutate: func(cfg *Config) { cfg.SubstrateConnector = "connector.castworks-system.svc:9091" },
			want:   "--substrate-connector",
		},
		{
			name:   "egress short name",
			mutate: func(cfg *Config) { cfg.SubstrateEgressHTTP = []string{"extsim:9100"} },
			want:   "--substrate-egress-http",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(cfg)
			p, err := newSubstrateProvider(context.Background(), cfg)
			if err == nil {
				t.Fatalf("newSubstrateProvider() error = nil, want an error for %q", tt.name)
			}
			if p != nil {
				t.Fatalf("newSubstrateProvider() provider = %v, want nil on error", p)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("newSubstrateProvider() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}
