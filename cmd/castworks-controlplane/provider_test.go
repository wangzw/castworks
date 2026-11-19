package main

import (
	"context"
	"fmt"
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
// 断言六个 --substrate-* 参数逐一对应。
func TestSubstrateProviderConfigMapsFlags(t *testing.T) {
	const (
		kubeconfig     = "/tmp/castworks-controlplane/kubeconfig"
		kubeContext    = "dev-context"
		atespace       = "atespace-test"
		controlPlane   = "control.example:9443"
		serviceAccount = "namespace-test/service-account-test"
	)
	args := append(configBaseArgs(ProviderSubstrate),
		"--substrate-kubeconfig", kubeconfig,
		"--substrate-context", kubeContext,
		"--substrate-atespace", atespace,
		"--substrate-image", providerTestImage,
		"--substrate-control-plane", controlPlane,
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
