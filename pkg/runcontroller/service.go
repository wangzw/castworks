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

// InboxKind 是收件箱事件的种类。
type InboxKind string

const (
	// KindHumanMessage 是人在 humanchannel 写入的消息。
	KindHumanMessage InboxKind = "human_message"
	// KindSubscriptionCompleted 是外部系统完成订阅后的通知。
	KindSubscriptionCompleted InboxKind = "subscription_completed"
)

// InboxEvent 是写入收件箱的一次事件。字段按种类使用，含义见
// docs/milestone/v0.2.0/runcontroller.md“接口”一节。
type InboxEvent struct {
	Kind         InboxKind
	Text         string                  // human_message：1 字节至 MaxTextBytes
	EventID      string                  // subscription_completed：必填
	DedupKey     string                  // subscription_completed：必填
	Subscription *SubscriptionCompletion // subscription_completed：必填
}

// SubscriptionCompletion 是订阅完成事件的负载，由 connectorhub 构造。
type SubscriptionCompletion struct {
	SubID              string
	System             string
	Event              string
	SubscriptionStatus string
	Terminal           string
	Correlation        map[string]string
	OccurredAt         time.Time
	Source             string
	Detail             string
	DetailTruncated    bool
}

type RunService interface {
	Create(ctx context.Context, prompt string) (runID string, err error)
	Get(ctx context.Context, runID string) (RunDetail, error)
	List(ctx context.Context) ([]RunSummary, error) // 按 CreatedAt 降序
	End(ctx context.Context, runID string) error
	AppendInbox(ctx context.Context, runID string, ev InboxEvent) (inboxSeq int64, duplicate bool, err error)
}

// MaxTextBytes 是单条 inbox 消息文本的最大字节数。它与
// agentprotocol 的校验上限含义相同，两处必须保持一致。
const MaxTextBytes = 64 << 10

// MaxDetailBytes 是订阅完成事件 Detail 字段的最大字节数。
const MaxDetailBytes = 48 << 10

// MaxCorrelationBytes 是订阅完成事件 Correlation 的键与值总字节数上限。
const MaxCorrelationBytes = 4 << 10

var (
	ErrNotFound     = errors.New("runcontroller: run not found")
	ErrTerminated   = errors.New("runcontroller: run ended or failed")
	ErrInvalidEvent = errors.New("runcontroller: invalid inbox event")
)

type Config struct {
	IdleGrace        time.Duration // G_idle，默认 30s
	ScanPeriod       time.Duration // P_scan，默认 5s
	HelloTimeout     time.Duration // T_hello，默认 60s
	ReconnectTimeout time.Duration // T_reconnect，默认 30s
	SuspendEnabled   bool          // false 时不运行挂起扫描
	Credential       LLMCredential
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
