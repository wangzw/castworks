package runcontroller

import (
	"context"
	"log/slog"
	"time"
)

// scanSource 提供扫描的候选查询，由 *store 实现。
type scanSource interface {
	suspendCandidates(ctx context.Context, idleBefore time.Time) ([]string, error)
	helloTimeoutCandidates(ctx context.Context, returnedBefore time.Time) ([]string, error)
}

// crashCandidate 是崩溃扫描选中的一个运行实例。disconnectRev 是选出时断线
// 记录的版本，事务内要据此确认期间没有重连。
type crashCandidate struct {
	runID         string
	disconnectRev uint64
}

// crashSource 选出出站流断开超过 T_reconnect 的 active 运行实例，由
// *Controller 结合存储层与内存断线记录实现。
type crashSource interface {
	crashCandidates(ctx context.Context, disconnectedBefore time.Time) ([]crashCandidate, error)
}

// scanTarget 执行单个运行实例的扫描动作，由 *Controller 实现。
type scanTarget interface {
	trySuspend(ctx context.Context, runID string) error
	expireHello(ctx context.Context, runID string) error
	tryFailCrashed(ctx context.Context, cand crashCandidate) error
}

// runScanLoop 每隔 cfg.ScanPeriod 执行一轮扫描：SuspendEnabled 为真时挂起
// 空闲的运行实例，无论是否启用挂起都检查 T_hello 超时与 actor 崩溃。ctx
// 取消后停表并返回 nil；单个运行实例的错误只记日志，不中断本轮。
func runScanLoop(ctx context.Context, cfg Config, clock Clock, src scanSource, crash crashSource, tgt scanTarget) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		timer := clock.NewTimer(cfg.ScanPeriod)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C():
		}

		scanRound(ctx, cfg, clock, src, crash, tgt)
	}
}

// scanRound 执行一轮扫描。同一轮的挂起、超时与崩溃检查使用同一个 now。
func scanRound(ctx context.Context, cfg Config, clock Clock, src scanSource, crash crashSource, tgt scanTarget) {
	now := clock.Now()
	if cfg.SuspendEnabled {
		scanCandidates(ctx, "suspend", now.Add(-cfg.IdleGrace), src.suspendCandidates, tgt.trySuspend)
	}
	scanCandidates(ctx, "hello timeout", now.Add(-cfg.HelloTimeout), src.helloTimeoutCandidates, tgt.expireHello)
	if cfg.ReconnectTimeout > 0 {
		scanCrashCandidates(ctx, now.Add(-cfg.ReconnectTimeout), crash, tgt)
	}
}

// scanCandidates 查询早于 before 的候选运行实例并逐个执行 apply。
// 查询失败只记日志；单个运行实例失败同样只记日志，其余照常执行。
func scanCandidates(
	ctx context.Context,
	kind string,
	before time.Time,
	query func(context.Context, time.Time) ([]string, error),
	apply func(context.Context, string) error,
) {
	if ctx.Err() != nil {
		return
	}

	runIDs, err := query(ctx, before)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("runcontroller: list scan candidates failed", "scan", kind, "error", err)
		return
	}

	for _, runID := range runIDs {
		if ctx.Err() != nil {
			return
		}
		if err := apply(ctx, runID); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("runcontroller: scan run failed", "scan", kind, "run_id", runID, "error", err)
		}
	}
}

// scanCrashCandidates 查询断开超过 T_reconnect 的 active 运行实例并逐个提交
// 崩溃检查。查询失败只记日志；单个运行实例失败同样只记日志。
func scanCrashCandidates(ctx context.Context, disconnectedBefore time.Time, src crashSource, tgt scanTarget) {
	if ctx.Err() != nil {
		return
	}

	candidates, err := src.crashCandidates(ctx, disconnectedBefore)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("runcontroller: list crash candidates failed", "error", err)
		return
	}

	for _, cand := range candidates {
		if ctx.Err() != nil {
			return
		}
		if err := tgt.tryFailCrashed(ctx, cand); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("runcontroller: crash scan failed", "run_id", cand.runID, "error", err)
		}
	}
}
