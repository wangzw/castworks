// Package agent 定义看守进程内 Claude Code 驱动、出站流客户端与 PID 1 职责
// 之间的接口。本包只有类型与接口，没有实现：Claude Code 驱动实现 Agent，
// 出站流客户端实现 Sink 并使用 Agent，PID 1 职责实现 ProcessRunner。
package agent

import (
	"context"
	"errors"
	"os/exec"
)

// Phase 是 Claude Code 驱动所处的阶段。
type Phase int

const (
	// PhaseStarting 表示 Claude Code 尚未启动。
	PhaseStarting Phase = iota + 1
	// PhaseInTurn 表示有一个回合正在进行。
	PhaseInTurn
	// PhaseBoundary 表示处于回合边界，没有进行中的回合。
	PhaseBoundary
	// PhaseExited 表示 Claude Code 已退出。
	PhaseExited
)

// TurnResult 是一个以回合边界结束的回合的结果。
type TurnResult struct {
	// TurnID 是回合标识，16 字节 crypto/rand 的小写十六进制。
	TurnID string
	// Output 是回合输出，已按 1 MiB 截断。
	Output string
	// IsError 表示回合以错误结束。
	IsError bool
}

// State 是驱动器状态的快照，出站流客户端据此构造 Hello 完成对账。
type State struct {
	// Phase 是当前阶段。
	Phase Phase
	// TurnID 是进行中回合的 turn_id，仅在 PhaseInTurn 时非空，其余为空。
	TurnID string
	// LastTurn 是最近一个以回合边界结束的回合；没有为 nil。
	LastTurn *TurnResult
	// LastAcceptedSeq 是已写入 Claude Code stdin 的最大 inbox_seq，未写入过为 0。
	LastAcceptedSeq int64
	// BgJobs 是当前的后台作业集合，任务标识按字典序排列；Claude Code 尚未
	// 启动或已退出时为空。
	BgJobs []string
}

// CredentialKind 是启动 Claude Code 所用凭据的种类。
type CredentialKind int

const (
	// CredentialAPIKey 表示 API key 凭据。
	CredentialAPIKey CredentialKind = iota + 1
	// CredentialOAuthToken 表示 OAuth token 凭据。
	CredentialOAuthToken
)

// Credential 是启动 Claude Code 时使用的 LLM 凭据。
type Credential struct {
	// Kind 是凭据种类。
	Kind CredentialKind
	// Value 是凭据值，不写入日志。
	Value string
}

// Batch 是一批待投递的消息。
type Batch struct {
	// DeliveryID 是投递标识。
	DeliveryID string
	// ThroughSeq 是这批消息覆盖到的最大 inbox_seq。
	ThroughSeq int64
	// Texts 是消息文本，已去重，按 inbox_seq 升序，非空。
	Texts []string
}

// Sink 接收驱动器在自己的状态锁内同步发出的事件。四个方法按调用顺序即
// 事件顺序；实现不得阻塞，不得回调 Agent。
type Sink interface {
	// Accepted 报告一批消息已写入 Claude Code stdin，throughSeq 是写入覆盖到的最大 inbox_seq。
	Accepted(deliveryID string, throughSeq int64)
	// TurnStarted 报告一个回合开始；自发回合与回合外退出的 turn_started 的 deliveryID 为空。
	TurnStarted(turnID, deliveryID, sessionID string)
	// TurnEnded 报告一个回合结束；agentExited 表示 Claude Code 已退出。
	TurnEnded(turnID, output string, isError, agentExited bool)
	// BgJobsChanged 报告后台作业集合发生变化。jobIDs 是变化之后的完整集合，
	// 按字典序排列；turnID 在回合中为当前回合，在回合边界上为空。
	BgJobsChanged(turnID string, jobIDs []string)
}

// Agent 是看守进程内 Claude Code 驱动的接口。
type Agent interface {
	// Snapshot 在驱动器的状态锁内调用 fn：fn 执行期间没有 Sink 回调穿插，fn 不得回调 Agent。
	Snapshot(fn func(State))
	// Deliver 由调用方串行调用，在写入完成或失败后返回，可能阻塞到 Claude Code 读走 stdin。
	// 回合边界上的投递发出 TurnStarted；回合进行中收到投递（含自发回合）时只回 Accepted，
	// 不另发 TurnStarted，由当前回合的 TurnEnded 一并结束。
	// cred 只在需要启动 Claude Code 时使用。
	Deliver(b Batch, cred Credential) error
	// Shutdown 按顺序关闭 Claude Code，进程退出后返回；Claude Code 未启动时立即返回 nil，
	// 可重复调用；开始关停之后不再调用 Sink。
	Shutdown(ctx context.Context) error
}

// ErrExited 表示 Claude Code 已退出，不能再接受投递。
var ErrExited = errors.New("agent: claude code has exited")

// ProcessRunner 启动子进程并报告其退出，调用方不得再调用 cmd.Wait。
type ProcessRunner interface {
	// Start 启动 cmd，进程退出时向 exited 发送一次退出结果（nil 或描述退出状态的错误）
	// 后关闭通道。调用方不得再调用 cmd.Wait；cmd.Stdin、cmd.Stdout、cmd.Stderr 只能是
	// *os.File 或 nil，避免依赖 Wait 的拷贝协程。
	Start(cmd *exec.Cmd) (exited <-chan error, err error)
}
