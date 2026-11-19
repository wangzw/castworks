// Package runcontroller 定义运行实例服务接口与运行控制器使用的类型。
package runcontroller

import (
	"context"
	"errors"
	"time"
)

type State string

const (
	StateProvisioning State = "provisioning"
	StateActive       State = "active"
	StateSuspending   State = "suspending"
	StateSuspended    State = "suspended"
	StateResuming     State = "resuming"
	StateEnded        State = "ended"
	StateFailed       State = "failed"
)

type Substate string

const (
	SubstateNone        Substate = ""
	SubstateInTurn      Substate = "in_turn"
	SubstateDelivering  Substate = "delivering"
	SubstateBoundary    Substate = "boundary"
	SubstateSnapshotted Substate = "snapshotted"
)

type RunSummary struct {
	RunID     string
	State     State
	Substate  Substate
	CreatedAt time.Time
}

type Turn struct {
	TurnID    string
	StartedAt time.Time
	EndedAt   *time.Time // 回合未结束为 nil
	Output    string
	IsError   bool
}

type Suspension struct {
	StartedAt  time.Time
	SnapshotMS *int64 // 未完成为 nil，下同
	RestoreMS  *int64
	HelloMS    *int64
	Error      string
}

type RunDetail struct {
	RunSummary
	FailureReason string
	Turns         []Turn       // 按 StartedAt 升序
	Suspensions   []Suspension // 按 StartedAt 升序
}

type RunService interface {
	Create(ctx context.Context, prompt string) (runID string, err error)
	Get(ctx context.Context, runID string) (RunDetail, error)
	List(ctx context.Context) ([]RunSummary, error) // 按 CreatedAt 降序
	End(ctx context.Context, runID string) error
	AppendInbox(ctx context.Context, runID string, text string) (inboxSeq int64, err error)
}

// MaxTextBytes 是单条 inbox 消息文本的最大字节数。它与
// agentprotocol 的校验上限含义相同，两处必须保持一致。
const MaxTextBytes = 64 << 10

var (
	ErrNotFound    = errors.New("runcontroller: run not found")
	ErrTerminated  = errors.New("runcontroller: run ended or failed")
	ErrInvalidText = errors.New("runcontroller: text empty or longer than 64 KiB")
)

type Config struct {
	IdleGrace      time.Duration // G_idle，默认 30s
	ScanPeriod     time.Duration // P_scan，默认 5s
	HelloTimeout   time.Duration // T_hello，默认 60s
	SuspendEnabled bool          // false 时不运行挂起扫描
	Credential     LLMCredential
}

type LLMCredentialKind string

const (
	LLMCredentialAPIKey     LLMCredentialKind = "api_key"
	LLMCredentialOAuthToken LLMCredentialKind = "oauth_token"
)

type LLMCredential struct {
	Kind  LLMCredentialKind
	Value string
}

type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

type realTimer struct {
	timer *time.Timer
}

// RealClock 返回使用系统时间的 Clock。
func RealClock() Clock {
	return realClock{}
}

func (realClock) Now() time.Time {
	return time.Now()
}

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{timer: time.NewTimer(d)}
}

func (t *realTimer) C() <-chan time.Time {
	return t.timer.C
}

func (t *realTimer) Stop() bool {
	return t.timer.Stop()
}
