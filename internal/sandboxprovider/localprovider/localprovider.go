// Package localprovider 提供不碰集群的沙箱提供者，用于看守进程本地模式的调试。
//
// 它不创建任何资源：Provision 与 Destroy 是空操作，GetSandbox 对任何 actor 都报告
// 运行中，快照相关操作一律返回 sandboxprovider.ErrUnsupported。实现不保存状态，
// 也不读取 ctx，因此天然并发安全。
package localprovider

import (
	"context"
	"fmt"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

// Provider 是本地提供者。它没有状态，零值即可使用。
type Provider struct{}

var _ sandboxprovider.Provider = (*Provider)(nil)

// New 返回本地提供者。
func New() sandboxprovider.Provider {
	return &Provider{}
}

// Provision 是空操作，返回零值 Timing 与 nil。
func (p *Provider) Provision(ctx context.Context, runID string) (sandboxprovider.Timing, error) {
	return sandboxprovider.Timing{}, nil
}

// Snapshot 不受本地提供者支持。
func (p *Provider) Snapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	return sandboxprovider.Timing{}, fmt.Errorf("localprovider: snapshot %q: %w", actor, sandboxprovider.ErrUnsupported)
}

// RestoreSnapshot 不受本地提供者支持。
func (p *Provider) RestoreSnapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	return sandboxprovider.Timing{}, fmt.Errorf("localprovider: restore snapshot %q: %w", actor, sandboxprovider.ErrUnsupported)
}

// Destroy 是空操作，返回 nil。
func (p *Provider) Destroy(ctx context.Context, actor string) error {
	return nil
}

// GetSandbox 对任何 actor 都报告运行中。
func (p *Provider) GetSandbox(ctx context.Context, actor string) (sandboxprovider.SandboxStatus, error) {
	return sandboxprovider.SandboxStatus{State: sandboxprovider.ActorStateRunning}, nil
}
