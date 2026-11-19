package main

import (
	"context"
	"fmt"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/localprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/substrate"
)

// newProvider 按 --provider 构造沙箱提供者。local 用不碰集群的本地提供者，
// substrate 用 substrate.New；后者的 ctx 只约束初始化。
func newProvider(ctx context.Context, cfg *Config) (sandboxprovider.Provider, error) {
	switch cfg.Provider {
	case ProviderLocal:
		return localprovider.New(), nil
	case ProviderSubstrate:
		provider, err := newSubstrateProvider(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// closeProvider 释放提供者。本地提供者没有需要释放的资源，只有实现了 Close
// 的提供者（Substrate）需要关闭。
func closeProvider(provider sandboxprovider.Provider) error {
	if closer, ok := provider.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// substrateProviderConfig 把 --substrate-* 六个参数逐一映射为 substrate.Config。
// 映射不做补全：--substrate-kubeconfig 与 --substrate-context 为空串时保持空串，
// 由 substrate 包按自己的默认加载规则处理，不在这里替换成默认路径。
func (c *Config) substrateProviderConfig() substrate.Config {
	return substrate.Config{
		Kubeconfig:     c.SubstrateKubeconfig,
		KubeContext:    c.SubstrateContext,
		Atespace:       c.SubstrateAtespace,
		Image:          c.SubstrateImage,
		ControlPlane:   c.SubstrateControlPlane,
		ServiceAccount: c.SubstrateServiceAccount,
	}
}

// newSubstrateProvider 用命令行配置构造 Substrate 提供者。ctx 只约束初始化，
// 后台的端口转发与令牌续签由返回值的 Close 结束。构造失败时返回 nil 与错误，
// 调用方据此终止启动，不监听任何端口。
func newSubstrateProvider(ctx context.Context, cfg *Config) (*substrate.Provider, error) {
	return substrate.New(ctx, cfg.substrateProviderConfig())
}
