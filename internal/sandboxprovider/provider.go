// Package sandboxprovider 定义 runcontroller 使用的沙箱提供者接口。
package sandboxprovider

import (
	"context"
	"errors"
	"time"
)

// ActorName 返回运行实例对应的 actor 名，即 "cw-" + runID。
func ActorName(runID string) string {
	return "cw-" + runID
}

type ActorState string

const (
	ActorStateUnknown   ActorState = "unknown"
	ActorStateRunning   ActorState = "running"
	ActorStateSuspended ActorState = "suspended"
	ActorStateResuming  ActorState = "resuming"
	ActorStateCrashed   ActorState = "crashed"
)

type SandboxStatus struct {
	State ActorState
}

type Timing struct {
	Duration      time.Duration // 提供者调用本身的耗时
	SnapshotBytes int64         // 仅 Snapshot 填写；取不到为 0
}

type Provider interface {
	Provision(ctx context.Context, runID string) (Timing, error)
	Snapshot(ctx context.Context, actor string) (Timing, error)
	RestoreSnapshot(ctx context.Context, actor string) (Timing, error)
	Destroy(ctx context.Context, actor string) error
	GetSandbox(ctx context.Context, actor string) (SandboxStatus, error)
}

var (
	ErrActorCrashed = errors.New("sandboxprovider: actor crashed")
	ErrNotFound     = errors.New("sandboxprovider: actor not found")
	ErrUnsupported  = errors.New("sandboxprovider: operation not supported")
)
