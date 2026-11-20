package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// substrateProviderConfig 把 --substrate-* 八个参数逐一映射为 substrate.Config。
// 映射不做补全：--substrate-kubeconfig 与 --substrate-context 为空串时保持空串，
// 由 substrate 包按自己的默认加载规则处理，不在这里替换成默认路径。
func (c *Config) substrateProviderConfig() substrate.Config {
	return substrate.Config{
		Kubeconfig:     c.SubstrateKubeconfig,
		KubeContext:    c.SubstrateContext,
		Atespace:       c.SubstrateAtespace,
		Image:          c.SubstrateImage,
		ControlPlane:   c.SubstrateControlPlane,
		Connector:      c.SubstrateConnector,
		EgressHTTP:     c.SubstrateEgressHTTP,
		ServiceAccount: c.SubstrateServiceAccount,
	}
}

// newSubstrateProvider 用命令行配置构造 Substrate 提供者。ctx 只约束初始化，
// 后台的端口转发与令牌续签由返回值的 Close 结束。构造失败时返回 nil 与错误，
// 调用方据此终止启动，不监听任何端口。错误中的 Config 字段名换成对应的
// 命令行参数名，启动者看到的是自己给出的参数。
func newSubstrateProvider(ctx context.Context, cfg *Config) (*substrate.Provider, error) {
	provider, err := substrate.New(ctx, cfg.substrateProviderConfig())
	if err != nil {
		return nil, substrateParamNameError(err)
	}
	return provider, nil
}

// substrateParamNames 把 substrate.Config 的字段名映射到设置它的命令行参数。
// 全限定名的校验在 substrate 包内，错误按 Config 字段名报告；控制面启动时
// 要指出的是参数名。
var substrateParamNames = []struct {
	field string
	param string
}{
	{"Config.ControlPlane", "--substrate-control-plane"},
	{"Config.Connector", "--substrate-connector"},
	{"Config.EgressHTTP", "--substrate-egress-http"},
}

// substrateParamNameError 把构造提供者时错误里的 Config 字段名换成参数名。
// 表里没有的字段（如 Config.Image）与不含字段名的错误原样返回。
func substrateParamNameError(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	rewritten := text
	for _, mapping := range substrateParamNames {
		rewritten = strings.ReplaceAll(rewritten, mapping.field, mapping.param)
	}
	if rewritten == text {
		return err
	}
	return errors.New(rewritten)
}
