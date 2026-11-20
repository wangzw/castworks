package connectorhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// defaultDeliverPeriod 是 P_deliver 的默认取值，即投递重试与取消扫描的周期。
const defaultDeliverPeriod = 5 * time.Second

// deliveryRuns 是投递与取消扫描需要运行实例服务提供的最小接口。它只用到
// main 上已有的 runcontroller.Get、RunDetail、ErrNotFound 与 ErrTerminated；
// AppendCompletion 接收本包的 Completion，由 Hub 对外接口工作项在适配层
// 转换成 runcontroller.InboxEvent 并接到 runcontroller.AppendInbox 上。
//
// duplicate 表示收件箱已按 dedup_key 判定了同一完成事件，与投递成功等价。
type deliveryRuns interface {
	Get(ctx context.Context, runID string) (runcontroller.RunDetail, error)
	AppendCompletion(ctx context.Context, runID string, c Completion) (duplicate bool, err error)
}

// deliverer 负责两件事：重投 completion_delivery 中 pending 的记录，以及把
// 已结束或已失败运行实例的活动订阅置为 cancelled。
//
// period 与 now 只由装配与测试注入：period 不大于 0 时取 defaultDeliverPeriod，
// now 为 nil 时取 time.Now。
type deliverer struct {
	pool   *pgxpool.Pool
	runs   deliveryRuns
	period time.Duration
	now    func() time.Time
}

// newDeliverer 构造一个投递器。
func newDeliverer(pool *pgxpool.Pool, runs deliveryRuns, period time.Duration, now func() time.Time) *deliverer {
	if period <= 0 {
		period = defaultDeliverPeriod
	}
	if now == nil {
		now = time.Now
	}
	return &deliverer{pool: pool, runs: runs, period: period, now: now}
}

// pendingDelivery 是 completion_delivery 中一条待投递的记录。
type pendingDelivery struct {
	subID    string
	runID    string
	dedupKey string
	event    []byte
}

// run 在启动时执行一次扫描，之后每隔 period 执行一次，ctx 取消后返回 nil。
// 单轮内的错误只记日志，不中断循环。
func (d *deliverer) run(ctx context.Context) error {
	d.scan(ctx)
	for {
		timer := time.NewTimer(d.period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		d.scan(ctx)
	}
}

// scan 执行一轮扫描：先重投全部 pending 记录，再取消已结束运行实例的活动
// 订阅。ctx 已取消时不再开始新的动作。
func (d *deliverer) scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := d.deliverPending(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("connectorhub: deliver pending completions failed", "error", err)
	}
	if ctx.Err() != nil {
		return
	}
	if err := d.cancelTerminated(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("connectorhub: cancel terminated subscriptions failed", "error", err)
	}
}

// deliverPending 重投全部 pending 的完成事件投递记录。调用方在匹配事务
// 提交之后立即调用它，即为“投递记录提交后立即投递”；后台扫描也调用同一
// 函数重投遗留的 pending 记录。
//
// 单条记录的暂时性错误记入 attempts 与 last_error，不返回错误；数据库或
// 记录内容的错误汇总返回，其余记录照常尝试。
func (d *deliverer) deliverPending(ctx context.Context) error {
	rows, err := d.pool.Query(ctx,
		`SELECT sub_id, run_id, dedup_key, event
		   FROM completion_delivery
		  WHERE status = 'pending'
		  ORDER BY created_at, sub_id, dedup_key`)
	if err != nil {
		return fmt.Errorf("connectorhub: list pending completions: %w", err)
	}

	var pending []pendingDelivery
	for rows.Next() {
		var p pendingDelivery
		if err := rows.Scan(&p.subID, &p.runID, &p.dedupKey, &p.event); err != nil {
			rows.Close()
			return fmt.Errorf("connectorhub: scan pending completion: %w", err)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("connectorhub: read pending completions: %w", err)
	}

	var errs []error
	for _, p := range pending {
		if ctx.Err() != nil {
			break
		}
		if err := d.deliverOne(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deliverOne 投递一条记录并按结果更新它的状态。
func (d *deliverer) deliverOne(ctx context.Context, p pendingDelivery) error {
	var completion Completion
	if err := json.Unmarshal(p.event, &completion); err != nil {
		return fmt.Errorf("connectorhub: decode completion for subscription %s: %w", p.subID, err)
	}

	_, err := d.runs.AppendCompletion(ctx, p.runID, completion)
	switch {
	case err == nil:
		// 成功与 duplicate 都表示收件箱已有该去重键的事件。
		return d.markDelivered(ctx, p)
	case errors.Is(err, runcontroller.ErrNotFound), errors.Is(err, runcontroller.ErrTerminated):
		return d.reject(ctx, p, err)
	default:
		return d.recordFailure(ctx, p, err)
	}
}

// markDelivered 把记录置为 delivered，并记入成功时刻。状态条件更新使并发的
// 重复扫描不覆盖对方的结果。
func (d *deliverer) markDelivered(ctx context.Context, p pendingDelivery) error {
	if _, err := d.pool.Exec(ctx,
		`UPDATE completion_delivery
		    SET status = 'delivered', attempts = attempts + 1,
		        last_error = '', delivered_at = $3
		  WHERE sub_id = $1 AND dedup_key = $2 AND status = 'pending'`,
		p.subID, p.dedupKey, d.now()); err != nil {
		return fmt.Errorf("connectorhub: mark completion %s delivered: %w", p.subID, err)
	}
	return nil
}

// reject 把记录置为 rejected，并在同一事务里把仍为 active 的订阅置为
// cancelled：运行实例已结束、已失败或不存在，完成事件不会再被消费。订阅
// 已 completed 时条件更新不命中，保持原状态。
func (d *deliverer) reject(ctx context.Context, p pendingDelivery, cause error) error {
	now := d.now()
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("connectorhub: begin reject transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx,
		`UPDATE completion_delivery
		    SET status = 'rejected', attempts = attempts + 1,
		        last_error = $3, delivered_at = $4
		  WHERE sub_id = $1 AND dedup_key = $2 AND status = 'pending'`,
		p.subID, p.dedupKey, cause.Error(), now); err != nil {
		return fmt.Errorf("connectorhub: mark completion %s rejected: %w", p.subID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE subscription
		    SET status = 'cancelled', completed_at = $2
		  WHERE sub_id = $1 AND status = 'active'`,
		p.subID, now); err != nil {
		return fmt.Errorf("connectorhub: cancel subscription %s: %w", p.subID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("connectorhub: commit completion rejection for %s: %w", p.subID, err)
	}
	return nil
}

// recordFailure 记录一次暂时性失败：保持 pending，attempts 加一，写入
// last_error，下一次扫描按同一 dedup_key 重试。
func (d *deliverer) recordFailure(ctx context.Context, p pendingDelivery, cause error) error {
	if _, err := d.pool.Exec(ctx,
		`UPDATE completion_delivery
		    SET attempts = attempts + 1, last_error = $3
		  WHERE sub_id = $1 AND dedup_key = $2 AND status = 'pending'`,
		p.subID, p.dedupKey, cause.Error()); err != nil {
		return fmt.Errorf("connectorhub: record completion %s failure: %w", p.subID, err)
	}
	return nil
}

// cancelTerminated 对仍有 active 订阅的运行实例逐个调用 Get。运行实例
// 不存在、已结束或已失败时，把它的 active 订阅以条件更新置为 cancelled。
// 其他错误视为暂时性错误，订阅保持 active，等待下一轮。
func (d *deliverer) cancelTerminated(ctx context.Context) error {
	rows, err := d.pool.Query(ctx,
		`SELECT DISTINCT run_id FROM subscription WHERE status = 'active' ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("connectorhub: list runs with active subscriptions: %w", err)
	}

	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return fmt.Errorf("connectorhub: scan run with active subscriptions: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("connectorhub: read runs with active subscriptions: %w", err)
	}

	var errs []error
	for _, runID := range runIDs {
		if ctx.Err() != nil {
			break
		}
		terminated, err := d.runTerminated(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("connectorhub: get run %s: %w", runID, err))
			continue
		}
		if !terminated {
			continue
		}
		if _, err := d.pool.Exec(ctx,
			`UPDATE subscription
			    SET status = 'cancelled', completed_at = $2
			  WHERE run_id = $1 AND status = 'active'`, runID, d.now()); err != nil {
			errs = append(errs, fmt.Errorf("connectorhub: cancel subscriptions of run %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}

// runTerminated 判断一个运行实例是否已结束或已失败。Get 返回 ErrNotFound
// 或 ErrTerminated 时同样视为终态；其他错误返回给调用方，订阅保持 active。
func (d *deliverer) runTerminated(ctx context.Context, runID string) (bool, error) {
	detail, err := d.runs.Get(ctx, runID)
	switch {
	case err == nil:
		return detail.State == runcontroller.StateEnded || detail.State == runcontroller.StateFailed, nil
	case errors.Is(err, runcontroller.ErrNotFound), errors.Is(err, runcontroller.ErrTerminated):
		return true, nil
	default:
		return false, err
	}
}
