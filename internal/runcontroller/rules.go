package runcontroller

import (
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
	"github.com/wangzw/castworks/internal/sandboxprovider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件是 runcontroller.md“状态机”与“行为”各小节、agentprotocol.md“对账”
// 一节与接口清单“自发回合”一节的纯函数实现。
//
// transition 不访问数据库、网络与时钟：时间一律取自 machineEnv.Now，
// 数据库写入、流的操作与提供者调用由调用方按返回的效果列表执行。

// machineState 是运行实例在状态机中的位置。字段与 run 表中参与规则的部分
// 一一对应，由控制器与 runRow 互转。
type machineState struct {
	State               State
	Substate            Substate
	NextInboxSeq        int64 // 下一条收件箱消息的序号，从 1 起
	DeliveredThrough    int64 // 已确认写入 Claude Code 的最大 inbox_seq
	IdleSince           *time.Time
	ProvisionReturnedAt *time.Time
	FailureReason       string
	EndedAt             *time.Time
}

func (s machineState) terminated() bool {
	return s.State == StateEnded || s.State == StateFailed
}

// 每条规则执行时的环境，不由运行实例行本身表示。
type machineEnv struct {
	Now             time.Time
	StreamConnected bool               // 该运行实例当前是否登记了流
	Suspension      *machineSuspension // 最近一次挂起记录，没有记录为 nil
}

// machineSuspension 是挂起记录中参与规则的两个时刻。
type machineSuspension struct {
	RestoreReturnedAt *time.Time // RestoreSnapshot 返回的时刻
	HelloAt           *time.Time // 恢复后收到 Hello 的时刻
}

// machineEvent 是触发一次状态转移的事件。
type machineEvent interface{ isMachineEvent() }

type evAppend struct{ text string }

type evProvisionDone struct{ err error }

type evHello struct {
	phase           agentprotocolv1.Phase
	turnID          string
	lastAcceptedSeq int64
	lastTurn        *agentprotocolv1.LastTurn
}

type evAccepted struct{ throughSeq int64 }

type evTurnStarted struct{ turnID, deliveryID string }

type evTurnEnded struct {
	turnID      string
	output      string
	isError     bool
	agentExited bool
}

type evStreamClosed struct{}

type evEnd struct{}

type evTrySuspend struct{ idleBefore time.Time }

type evSnapshotDone struct {
	timing  sandboxprovider.Timing
	err     error
	crashed bool
}

type evRestoreDone struct {
	timing sandboxprovider.Timing
	err    error
}

type evHelloExpire struct{ returnedBefore time.Time }

func (evAppend) isMachineEvent()        {}
func (evProvisionDone) isMachineEvent() {}
func (evHello) isMachineEvent()         {}
func (evAccepted) isMachineEvent()      {}
func (evTurnStarted) isMachineEvent()   {}
func (evTurnEnded) isMachineEvent()     {}
func (evStreamClosed) isMachineEvent()  {}
func (evEnd) isMachineEvent()           {}
func (evTrySuspend) isMachineEvent()    {}
func (evSnapshotDone) isMachineEvent()  {}
func (evRestoreDone) isMachineEvent()   {}
func (evHelloExpire) isMachineEvent()   {}

// machineEffect 是调用方在提交后按顺序执行的一个动作。效果种类是闭集。
type machineEffect interface{ isMachineEffect() }

type insertInbox struct {
	seq  int64
	text string
}

type turnStarted struct {
	turnID     string
	deliveryID string
	at         time.Time
}

type turnEnded struct {
	turnID  string
	output  string
	isError bool
	at      time.Time
}

type openSuspension struct{ at time.Time }

// updateSuspension 只写其中非 nil 的字段，对应
// (*storeTx).updateLatestSuspension 的“只写给出的字段”。
type updateSuspension struct {
	snapshotMS        *int64
	snapshotBytes     *int64
	resumeRequestedAt *time.Time
	restoreMS         *int64
	restoreReturnedAt *time.Time
	helloAt           *time.Time
	helloMS           *int64
	err               *string
}

type registerStream struct{}

type unregisterStream struct{}

type sendHelloAck struct{}

type deliver struct{ from, through int64 }

type sendShutdown struct{}

type closeStream struct{ code codes.Code }

type callProvision struct{}

type callSnapshot struct{}

type callRestore struct{}

type callDestroy struct{}

func (insertInbox) isMachineEffect()      {}
func (turnStarted) isMachineEffect()      {}
func (turnEnded) isMachineEffect()        {}
func (openSuspension) isMachineEffect()   {}
func (updateSuspension) isMachineEffect() {}
func (registerStream) isMachineEffect()   {}
func (unregisterStream) isMachineEffect() {}
func (sendHelloAck) isMachineEffect()     {}
func (deliver) isMachineEffect()          {}
func (sendShutdown) isMachineEffect()     {}
func (closeStream) isMachineEffect()      {}
func (callProvision) isMachineEffect()    {}
func (callSnapshot) isMachineEffect()     {}

// callProvision 由控制器在 Create 提交后直接执行，规则本身目前不产生它。
// 它仍是效果闭集的一员，这里用接口断言保留并在编译期校验。
var _ machineEffect = callProvision{}

func (callRestore) isMachineEffect() {}
func (callDestroy) isMachineEffect() {}

// machineResult 是转移后的新状态与按执行顺序排列的效果。
type machineResult struct {
	State   machineState
	Effects []machineEffect
}

func mkResult(s machineState, effects ...machineEffect) machineResult {
	return machineResult{State: s, Effects: effects}
}

// transition 计算一次状态转移。它不修改输入，也不访问外部状态。
//
// 终态上不允许的外部事件返回 ErrTerminated；协议违例返回
// codes.InvalidArgument 的错误；其余在当前状态下无事可做的事件返回
// 空效果且不报错。
func transition(cur machineState, ev machineEvent, env machineEnv) (machineResult, error) {
	switch e := ev.(type) {
	case evAppend:
		return transitionAppend(cur, e, env)
	case evProvisionDone:
		return transitionProvisionDone(cur, e, env)
	case evHello:
		return transitionHello(cur, e, env)
	case evAccepted:
		return transitionAccepted(cur, e, env)
	case evTurnStarted:
		return transitionTurnStarted(cur, e, env)
	case evTurnEnded:
		return transitionTurnEnded(cur, e, env)
	case evStreamClosed:
		return transitionStreamClosed(cur, e, env)
	case evEnd:
		return transitionEnd(cur, e, env)
	case evTrySuspend:
		return transitionTrySuspend(cur, e, env)
	case evSnapshotDone:
		return transitionSnapshotDone(cur, e, env)
	case evRestoreDone:
		return transitionRestoreDone(cur, e, env)
	case evHelloExpire:
		return transitionHelloExpire(cur, e, env)
	default:
		return mkResult(cur), nil
	}
}

// maybeDeliver 在 active 状态、有未投递消息且流已连接时产生一批投递，
// 并把子状态改为 delivering。消息序号从 DeliveredThrough+1 到已分配的最大序号。
func maybeDeliver(cur *machineState, env machineEnv) (machineEffect, bool) {
	if cur.State != StateActive || !env.StreamConnected {
		return nil, false
	}
	through := cur.NextInboxSeq - 1
	if through <= cur.DeliveredThrough {
		return nil, false
	}
	cur.Substate = SubstateDelivering
	return deliver{from: cur.DeliveredThrough + 1, through: through}, true
}

func transitionAppend(cur machineState, e evAppend, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), ErrTerminated
	}
	seq := cur.NextInboxSeq
	if seq < 1 {
		seq = 1
	}
	cur.NextInboxSeq = seq + 1
	effects := []machineEffect{insertInbox{seq: seq, text: e.text}}

	switch cur.State {
	case StateActive:
		// 新消息与恢复表：active/boundary 提交后投递；
		// active/in_turn 与 active/delivering 只写入，回合结束后投递。
		if cur.Substate == SubstateBoundary {
			if d, ok := maybeDeliver(&cur, env); ok {
				effects = append(effects, d)
			}
		}
	case StateSuspended:
		// 同一事务转 resuming，提交后调用 RestoreSnapshot。
		now := env.Now
		cur.State = StateResuming
		cur.Substate = SubstateSnapshotted
		effects = append(effects,
			updateSuspension{resumeRequestedAt: &now},
			callRestore{},
		)
	}
	// provisioning、resuming：只写入，收到 Hello 后投递。
	// suspending：只写入，由挂起第 3 步发现并恢复。
	return mkResult(cur, effects...), nil
}

func transitionProvisionDone(cur machineState, e evProvisionDone, env machineEnv) (machineResult, error) {
	// 提供者调用的完成事件在运行实例已结束时什么也不做。
	if cur.terminated() {
		return mkResult(cur), nil
	}
	if e.err == nil {
		// 成功时写 provision_returned_at，不改状态：状态由 Hello 推进，
		// Hello 可能早于 Provision 返回。
		now := env.Now
		cur.ProvisionReturnedAt = &now
		return mkResult(cur), nil
	}
	if cur.State == StateProvisioning {
		cur.State = StateFailed
		cur.Substate = SubstateNone
		cur.FailureReason = e.err.Error()
		now := env.Now
		cur.EndedAt = &now
	}
	return mkResult(cur), nil
}

func transitionHello(cur machineState, e evHello, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), ErrTerminated
	}

	// Hello 本身就是新流的登记：处理它时一律视为已连接，不看
	// env.StreamConnected。否则首帧 Hello 与旧流注销后的重连都不会投递，
	// 因为新流正是在这次事件里才登记的。
	env.StreamConnected = true

	switch e.phase {
	case agentprotocolv1.Phase_PHASE_STARTING,
		agentprotocolv1.Phase_PHASE_BOUNDARY,
		agentprotocolv1.Phase_PHASE_IN_TURN,
		agentprotocolv1.Phase_PHASE_EXITED:
	default:
		return mkResult(cur), status.Errorf(codes.InvalidArgument,
			"runcontroller: hello agent_state phase %v is not defined", e.phase)
	}

	maxSeq := cur.NextInboxSeq - 1
	if e.lastAcceptedSeq < 0 || e.lastAcceptedSeq > maxSeq {
		return mkResult(cur), status.Errorf(codes.InvalidArgument,
			"runcontroller: hello last_accepted_seq %d is outside [0, %d]", e.lastAcceptedSeq, maxSeq)
	}
	if e.lastAcceptedSeq > cur.DeliveredThrough {
		cur.DeliveredThrough = e.lastAcceptedSeq
	}

	// 对账：回合开始与结果由 Hello 补齐。
	var effects []machineEffect
	switch e.phase {
	case agentprotocolv1.Phase_PHASE_IN_TURN:
		if e.turnID != "" {
			effects = append(effects, turnStarted{turnID: e.turnID, at: env.Now})
		}
	case agentprotocolv1.Phase_PHASE_BOUNDARY:
		if e.lastTurn != nil && e.lastTurn.GetTurnId() != "" {
			effects = append(effects, turnEnded{
				turnID:  e.lastTurn.GetTurnId(),
				output:  e.lastTurn.GetOutput(),
				isError: e.lastTurn.GetIsError(),
				at:      env.Now,
			})
		}
	}

	switch cur.State {
	case StateSuspending, StateSuspended:
		// 只登记这条流、更新位置与回合记录，不改变状态、不投递。
		effects = append(effects, registerStream{}, sendHelloAck{})
		return mkResult(cur, effects...), nil

	case StateProvisioning, StateActive, StateResuming:
		if cur.State == StateResuming {
			now := env.Now
			var helloMS int64
			if env.Suspension != nil && env.Suspension.RestoreReturnedAt != nil &&
				!now.Before(*env.Suspension.RestoreReturnedAt) {
				helloMS = now.Sub(*env.Suspension.RestoreReturnedAt).Milliseconds()
			}
			effects = append(effects, updateSuspension{helloAt: &now, helloMS: &helloMS})
		}

		switch e.phase {
		case agentprotocolv1.Phase_PHASE_EXITED:
			// 看守进程报告 Claude Code 已经退出：转 failed 并以
			// FailedPrecondition 关闭这条流，不登记、不应答。
			cur.State = StateFailed
			cur.Substate = SubstateNone
			cur.FailureReason = "agent exited"
			now := env.Now
			cur.EndedAt = &now
			effects = append(effects, closeStream{code: codes.FailedPrecondition})
			return mkResult(cur, effects...), nil

		case agentprotocolv1.Phase_PHASE_IN_TURN:
			cur.State = StateActive
			cur.Substate = SubstateInTurn
			effects = append(effects, registerStream{}, sendHelloAck{})

		default: // STARTING 与 BOUNDARY
			cur.State = StateActive
			cur.Substate = SubstateBoundary
			now := env.Now
			cur.IdleSince = &now
			effects = append(effects, registerStream{}, sendHelloAck{})
			if d, ok := maybeDeliver(&cur, env); ok {
				effects = append(effects, d)
			}
		}
	}
	return mkResult(cur, effects...), nil
}

func transitionAccepted(cur machineState, e evAccepted, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), ErrTerminated
	}
	maxSeq := cur.NextInboxSeq - 1
	if e.throughSeq < 1 || e.throughSeq > maxSeq {
		return mkResult(cur), status.Errorf(codes.InvalidArgument,
			"runcontroller: accepted through_seq %d is outside [1, %d]", e.throughSeq, maxSeq)
	}
	if e.throughSeq > cur.DeliveredThrough {
		cur.DeliveredThrough = e.throughSeq
	}
	if cur.State == StateActive && cur.Substate == SubstateDelivering {
		cur.Substate = SubstateInTurn
	}
	return mkResult(cur), nil
}

func transitionTurnStarted(cur machineState, e evTurnStarted, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), ErrTerminated
	}
	effects := []machineEffect{turnStarted{turnID: e.turnID, deliveryID: e.deliveryID, at: env.Now}}
	// 子状态只有 boundary 转 in_turn；delivering 与 in_turn 不变。
	// 非 active 的非终态只写回合记录。
	if cur.State == StateActive && cur.Substate == SubstateBoundary {
		cur.Substate = SubstateInTurn
	}
	return mkResult(cur, effects...), nil
}

func transitionTurnEnded(cur machineState, e evTurnEnded, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), ErrTerminated
	}
	effects := []machineEffect{turnEnded{
		turnID:  e.turnID,
		output:  e.output,
		isError: e.isError,
		at:      env.Now,
	}}
	if cur.State != StateActive {
		// 非 active 的非终态只写回合记录，不改状态。
		return mkResult(cur, effects...), nil
	}
	if e.agentExited {
		// 任何 active 子状态下 Claude Code 退出都转 failed。
		cur.State = StateFailed
		cur.Substate = SubstateNone
		cur.FailureReason = "agent exited"
		now := env.Now
		cur.EndedAt = &now
		return mkResult(cur, effects...), nil
	}
	if cur.Substate == SubstateInTurn {
		// 回合结束回到边界，随后检查有无未投递消息。
		cur.Substate = SubstateBoundary
		now := env.Now
		cur.IdleSince = &now
		if d, ok := maybeDeliver(&cur, env); ok {
			effects = append(effects, d)
		}
	}
	// delivering 不变，等 Accepted；boundary 保持边界。
	return mkResult(cur, effects...), nil
}

func transitionStreamClosed(cur machineState, _ evStreamClosed, env machineEnv) (machineResult, error) {
	// 流的关闭不是外部事件：终态下同样只是把登记去掉。
	effects := []machineEffect{unregisterStream{}}
	if cur.terminated() {
		return mkResult(cur, effects...), nil
	}
	// 只把 delivering 退回 boundary；其余子状态不变。
	if cur.State == StateActive && cur.Substate == SubstateDelivering {
		cur.Substate = SubstateBoundary
		now := env.Now
		cur.IdleSince = &now
	}
	return mkResult(cur, effects...), nil
}

func transitionEnd(cur machineState, _ evEnd, env machineEnv) (machineResult, error) {
	switch cur.State {
	case StateEnded:
		// 重复调用不报错，也不再有动作。
		return mkResult(cur), nil
	case StateFailed:
		// failed 保持 failed，照常排 Destroy 删除 actor 释放 worker。
		return mkResult(cur, callDestroy{}), nil
	}

	cur.State = StateEnded
	cur.Substate = SubstateNone
	now := env.Now
	cur.EndedAt = &now
	cur.IdleSince = nil

	var effects []machineEffect
	if env.StreamConnected {
		// 先入队 Shutdown，再以 FailedPrecondition 关闭这条流。
		effects = append(effects, sendShutdown{}, closeStream{code: codes.FailedPrecondition})
	}
	effects = append(effects, callDestroy{})
	return mkResult(cur, effects...), nil
}

func transitionTrySuspend(cur machineState, e evTrySuspend, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), nil
	}
	// 只挂起 active/boundary、无未投递消息且 idle_since 早于截止时刻的运行实例。
	if cur.State != StateActive || cur.Substate != SubstateBoundary {
		return mkResult(cur), nil
	}
	if cur.DeliveredThrough < cur.NextInboxSeq-1 {
		return mkResult(cur), nil
	}
	if cur.IdleSince == nil || !cur.IdleSince.Before(e.idleBefore) {
		return mkResult(cur), nil
	}
	cur.State = StateSuspending
	cur.Substate = SubstateNone
	return mkResult(cur,
		openSuspension{at: env.Now},
		callSnapshot{},
	), nil
}

func transitionSnapshotDone(cur machineState, e evSnapshotDone, env machineEnv) (machineResult, error) {
	// 提供者调用的完成事件在运行实例已结束时什么也不做；非 suspending
	// 状态说明这是一个过期的完成事件。两种情况都无效果。
	if cur.terminated() || cur.State != StateSuspending {
		return mkResult(cur), nil
	}

	if e.err != nil {
		msg := e.err.Error()
		effects := []machineEffect{updateSuspension{err: &msg}}
		if e.crashed {
			cur.State = StateFailed
			cur.Substate = SubstateNone
			cur.FailureReason = msg
			now := env.Now
			cur.EndedAt = &now
			return mkResult(cur, effects...), nil
		}
		// 未崩溃：退回边界，随后投递挂起期间写入的消息。
		cur.State = StateActive
		cur.Substate = SubstateBoundary
		now := env.Now
		cur.IdleSince = &now
		if d, ok := maybeDeliver(&cur, env); ok {
			effects = append(effects, d)
		}
		return mkResult(cur, effects...), nil
	}

	ms := e.timing.Duration.Milliseconds()
	bytes := e.timing.SnapshotBytes
	upd := updateSuspension{snapshotMS: &ms, snapshotBytes: &bytes}

	if cur.DeliveredThrough < cur.NextInboxSeq-1 {
		// 快照完成时已有未投递消息：直接转 resuming 并恢复。
		cur.State = StateResuming
		cur.Substate = SubstateSnapshotted
		now := env.Now
		upd.resumeRequestedAt = &now
		return mkResult(cur, upd, callRestore{}), nil
	}
	cur.State = StateSuspended
	cur.Substate = SubstateSnapshotted
	return mkResult(cur, upd), nil
}

func transitionRestoreDone(cur machineState, e evRestoreDone, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), nil
	}
	// 恢复期间（resuming）或 Hello 已先到（active）时才有意义。
	if cur.State != StateResuming && cur.State != StateActive {
		return mkResult(cur), nil
	}
	if e.err != nil {
		msg := e.err.Error()
		cur.State = StateFailed
		cur.Substate = SubstateNone
		cur.FailureReason = msg
		now := env.Now
		cur.EndedAt = &now
		return mkResult(cur, updateSuspension{err: &msg}), nil
	}
	ms := e.timing.Duration.Milliseconds()
	now := env.Now
	return mkResult(cur, updateSuspension{restoreMS: &ms, restoreReturnedAt: &now}), nil
}

func transitionHelloExpire(cur machineState, e evHelloExpire, env machineEnv) (machineResult, error) {
	if cur.terminated() {
		return mkResult(cur), nil
	}
	switch cur.State {
	case StateProvisioning:
		if cur.ProvisionReturnedAt == nil || !cur.ProvisionReturnedAt.Before(e.returnedBefore) {
			return mkResult(cur), nil
		}
	case StateResuming:
		// 规则内复查：只有最近一次挂起记录确已返回、返回时刻早于截止
		// 时刻且尚未收到 Hello 时才超时。事务外查到的候选可能在事务内
		// 已经变化，已转 active 或已记录 Hello 的运行实例不能转 failed。
		if env.Suspension == nil || env.Suspension.RestoreReturnedAt == nil ||
			!env.Suspension.RestoreReturnedAt.Before(e.returnedBefore) ||
			env.Suspension.HelloAt != nil {
			return mkResult(cur), nil
		}
	default:
		return mkResult(cur), nil
	}
	cur.State = StateFailed
	cur.Substate = SubstateNone
	cur.FailureReason = "hello timeout"
	now := env.Now
	cur.EndedAt = &now
	return mkResult(cur), nil
}
