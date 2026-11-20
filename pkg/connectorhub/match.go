package connectorhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// connectorEvent 是 connector_event 表中供匹配使用的一行。
type connectorEvent struct {
	AppendSeq   int64
	System      string
	DeliveryID  string
	EventType   string
	ProjectIDs  []string
	Correlation map[string]string
	OccurredAt  time.Time
	Payload     []byte
}

// subscription 是 subscription 表的一行。DedupSpec 保存登记时的
// events.dedup_key 副本，即去重字段路径的列表。
type subscription struct {
	SubID       string
	RunID       string
	ProjectID   string
	System      string
	Event       string
	Correlation map[string]string
	Status      string
	Terminal    string
	SinceCursor int64
	DedupSpec   []string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

const subscriptionSelectColumns = `sub_id, run_id, project_id, system, event, correlation,
	status, terminal, since_cursor, dedup_spec, created_at, completed_at`

// rowScanner 是 pgx.Row 与 pgx.Rows 共有的 Scan 方法。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubscription(row rowScanner) (subscription, error) {
	var sub subscription
	if err := row.Scan(
		&sub.SubID, &sub.RunID, &sub.ProjectID, &sub.System, &sub.Event,
		&sub.Correlation, &sub.Status, &sub.Terminal, &sub.SinceCursor,
		&sub.DedupSpec, &sub.CreatedAt, &sub.CompletedAt,
	); err != nil {
		return subscription{}, err
	}
	return sub, nil
}

// matchEvent 把一条连接器事件与全部活动订阅匹配，在调用方给出的事务内
// 完成命中的订阅并写认领记录与 pending 的投递记录，返回完成的订阅数。
//
// terminal 由连接器从事件负载取出的结论字段。事件的 project_ids 为空时
// 不参与匹配。每个命中的订阅以状态条件更新裁决：更新失败表示订阅已由其
// 他路径完成或取消，该订阅不写任何记录。
func matchEvent(ctx context.Context, tx pgx.Tx, ev connectorEvent, terminal string, now time.Time) (int, error) {
	if len(ev.ProjectIDs) == 0 {
		return 0, nil
	}
	if terminal == "" {
		return 0, errors.New("connectorhub: match event without a terminal value")
	}

	correlation, err := encodeCorrelation(ev.Correlation)
	if err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT `+subscriptionSelectColumns+` FROM subscription
		 WHERE status = 'active'
		   AND system = $1
		   AND event = $2
		   AND project_id = ANY($3)
		   AND correlation <@ $4::jsonb
		 ORDER BY created_at, sub_id`,
		ev.System, ev.EventType, ev.ProjectIDs, correlation)
	if err != nil {
		return 0, fmt.Errorf("connectorhub: find matching subscriptions: %w", err)
	}

	var candidates []subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("connectorhub: scan matching subscription: %w", err)
		}
		candidates = append(candidates, sub)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("connectorhub: read matching subscriptions: %w", err)
	}

	completed := 0
	for _, sub := range candidates {
		ok, err := completeWithEvent(ctx, tx, sub, ev, terminal, now)
		if err != nil {
			return completed, err
		}
		if ok {
			completed++
		}
	}
	return completed, nil
}

// matchSubscription 为一个活动订阅扫描 append_seq 在 [fromSeq, toSeq) 内、
// 尚未被该订阅认领的匹配事件，命中时按 occurred_at 最早的一条完成订阅。
// 回溯取 [1, since_cursor)，登记后重扫取 [since_cursor, 上界)。
//
// extractTerminal 由连接器从事件负载取出结论字段。订阅不是 active、区间
// 为空或没有匹配事件时返回 false，不写任何记录。
func matchSubscription(
	ctx context.Context,
	tx pgx.Tx,
	sub subscription,
	fromSeq, toSeq int64,
	now time.Time,
	extractTerminal func([]byte) (string, error),
) (bool, error) {
	if sub.Status != "active" || fromSeq >= toSeq {
		return false, nil
	}

	correlation, err := encodeCorrelation(sub.Correlation)
	if err != nil {
		return false, err
	}

	var ev connectorEvent
	err = tx.QueryRow(ctx,
		`SELECT e.append_seq, e.system, e.delivery_id, e.event_type,
		        e.project_ids, e.correlation, e.occurred_at, e.payload
		   FROM connector_event e
		  WHERE e.append_seq >= $1
		    AND e.append_seq < $2
		    AND e.system = $3
		    AND e.event_type = $4
		    AND $5 = ANY(e.project_ids)
		    AND e.correlation @> $6::jsonb
		    AND NOT EXISTS (
		        SELECT 1 FROM subscription_claim c
		         WHERE c.sub_id = $7 AND c.delivery_id = e.delivery_id)
		  ORDER BY e.occurred_at, e.append_seq
		  LIMIT 1`,
		fromSeq, toSeq, sub.System, sub.Event, sub.ProjectID, correlation, sub.SubID,
	).Scan(
		&ev.AppendSeq, &ev.System, &ev.DeliveryID, &ev.EventType,
		&ev.ProjectIDs, &ev.Correlation, &ev.OccurredAt, &ev.Payload,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("connectorhub: find events for subscription %s: %w", sub.SubID, err)
	}

	terminal, err := extractTerminal(ev.Payload)
	if err != nil {
		return false, fmt.Errorf("connectorhub: extract terminal for event %s: %w", ev.DeliveryID, err)
	}
	return completeWithEvent(ctx, tx, sub, ev, terminal, now)
}

// completeFromStatus 以状态查询结果完成一个订阅：条件更新为 completed，
// 写一条 source 为 verify 的 pending 投递记录，返回是否完成。订阅已由其
// 他路径完成或取消时返回 false，不写任何记录。
func completeFromStatus(
	ctx context.Context,
	tx pgx.Tx,
	sub subscription,
	terminal string,
	detail []byte,
	now time.Time,
) (bool, error) {
	if terminal == "" {
		return false, errors.New("connectorhub: status query without a terminal value")
	}

	ok, err := casCompleteSubscription(ctx, tx, sub.SubID, terminal, now)
	if err != nil || !ok {
		return false, err
	}

	completion := newVerifyCompletion(sub, terminal, detail, now)
	if err := insertCompletionDelivery(ctx, tx, sub.RunID, completion, now); err != nil {
		return false, err
	}
	return true, nil
}

// completeWithEvent 以一条匹配事件完成订阅。先做状态条件更新，成功才写
// 认领记录与投递记录；更新失败时事务里没有该订阅的任何新记录。
func completeWithEvent(
	ctx context.Context,
	tx pgx.Tx,
	sub subscription,
	ev connectorEvent,
	terminal string,
	now time.Time,
) (bool, error) {
	ok, err := casCompleteSubscription(ctx, tx, sub.SubID, terminal, now)
	if err != nil || !ok {
		return false, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO subscription_claim (sub_id, delivery_id) VALUES ($1, $2)`,
		sub.SubID, ev.DeliveryID); err != nil {
		return false, fmt.Errorf("connectorhub: claim event %s for subscription %s: %w", ev.DeliveryID, sub.SubID, err)
	}

	completion := newWebhookCompletion(sub, ev, terminal)
	if err := insertCompletionDelivery(ctx, tx, sub.RunID, completion, now); err != nil {
		return false, err
	}
	return true, nil
}

// encodeCorrelation 把关联键编码为 JSON 对象。nil 映射编码为 {}，使
// jsonb 包含判断有确定的结果。
func encodeCorrelation(correlation map[string]string) (string, error) {
	if correlation == nil {
		return "{}", nil
	}
	data, err := json.Marshal(correlation)
	if err != nil {
		return "", fmt.Errorf("connectorhub: encode correlation: %w", err)
	}
	return string(data), nil
}
