package connectorhub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

const (
	// maxSubscribeBodyBytes 是订阅登记请求体的读取上限。请求体只有三个短
	// 字段，这个上限只用来避免无界读取。
	maxSubscribeBodyBytes = 1 << 20

	// maxAppendSeq 是登记之后重扫的上界。重扫不设真正的上界，取 int64 的
	// 最大值即可。
	maxAppendSeq int64 = 1<<63 - 1

	// subscriptionIDLength 是 sub_id 随机部分的小写 base32 字符数。
	subscriptionIDLength = 12
)

// subscribeRequest 是 POST /v1/runs/{run_id}/subscriptions 的请求体。
type subscribeRequest struct {
	System      string            `json:"system"`
	Event       string            `json:"event"`
	Correlation map[string]string `json:"correlation"`
}

// subscribeResponse 是订阅登记的 201 响应体。
type subscribeResponse struct {
	SubID       string `json:"sub_id"`
	SinceCursor int64  `json:"since_cursor"`
	Status      string `json:"status"`
}

// handleSubscribe 按 connectorhub.md“订阅登记”一节的六步登记一个 once 订阅。
func (h *connectorHandler) handleSubscribe(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	// 第一步先确认运行实例存在且未结束，与“订阅登记”一节的步骤一致。
	if status, message := h.checkRun(ctx, runID); status != 0 {
		writeError(w, status, message)
		return
	}
	request, status, message := decodeSubscribeRequest(w, r)
	if status != 0 {
		writeError(w, status, message)
		return
	}

	system, completion, correlation, status, message := h.validateSubscribe(request)
	if status != 0 {
		writeError(w, status, message)
		return
	}

	subID, err := newSubscriptionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := h.now()
	sub := subscription{
		SubID:       subID,
		RunID:       runID,
		ProjectID:   h.config.Project,
		System:      system.Name,
		Event:       request.Event,
		Correlation: correlation,
		Status:      "active",
		DedupSpec:   append([]string{}, system.Definition.Events.DedupKey...),
		CreatedAt:   now,
	}

	sinceCursor, err := h.registerSubscription(ctx, sub, now, system)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	sub.SinceCursor = sinceCursor

	// 重扫覆盖快照之后、登记返回之前提交的事件。
	if h.afterSnapshot != nil {
		h.afterSnapshot()
	}
	if err := h.rescanSubscription(ctx, sub, now, system); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	current, err := readSubscription(ctx, h.pool, subID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 订阅仍为 active 时以状态查询兜底。不可达或未到终态时订阅保持 active。
	if current.Status == "active" && completion.Verify != nil {
		queriedAt := h.now()
		terminal, reached, detail, err := system.Connector.CheckStatus(
			ctx, completion.Verify.API, current.Correlation)
		if err == nil && reached {
			if err := h.completeFromVerify(ctx, current, terminal, detail, queriedAt); err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			current, err = readSubscription(ctx, h.pool, subID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}

	writeJSON(w, http.StatusCreated, subscribeResponse{
		SubID:       subID,
		SinceCursor: sinceCursor,
		Status:      current.Status,
	})
}

// decodeSubscribeRequest 读取并解析订阅登记请求体。不是 JSON、字段类型不对
// 或带有尾随内容时返回 400。
func decodeSubscribeRequest(w http.ResponseWriter, r *http.Request) (subscribeRequest, int, string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscribeBodyBytes))
	if err != nil {
		return subscribeRequest{}, http.StatusBadRequest, "request body is not readable"
	}

	var request subscribeRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&request); err != nil {
		return subscribeRequest{}, http.StatusBadRequest, "request body is not valid JSON"
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return subscribeRequest{}, http.StatusBadRequest, "request body has trailing data"
	}
	return request, 0, ""
}

// validateSubscribe 校验订阅参数，不满足时返回 400 与一条面向 Agent 的
// 英文说明。校验顺序与 connectorhub.md“connector 端点”一节的状态码表一致：
// 外部系统可用、事件在完成条件目录中、关联键属于定义、至少含一个身份键、
// 项目归属键的取值属于本项目的绑定值。返回的关联键已按定义规范化。
func (h *connectorHandler) validateSubscribe(request subscribeRequest) (*System, *CompletionCondition, map[string]string, int, string) {
	if request.System == "" {
		return nil, nil, nil, http.StatusBadRequest, "system is required"
	}
	system, ok := h.config.System(request.System)
	if !ok {
		return nil, nil, nil, http.StatusBadRequest,
			fmt.Sprintf("external system %q is not available", request.System)
	}
	if request.Event == "" {
		return nil, nil, nil, http.StatusBadRequest, "event is required"
	}
	completion, ok := system.Definition.CompletionFor(request.Event)
	if !ok {
		return nil, nil, nil, http.StatusBadRequest,
			fmt.Sprintf("event %q is not in the event catalogue of system %q", request.Event, request.System)
	}

	correlationKeys := make(map[string]bool, len(system.Definition.Events.CorrelationKeys))
	for _, key := range system.Definition.Events.CorrelationKeys {
		correlationKeys[key] = true
	}
	correlationBytes := 0
	for key, value := range request.Correlation {
		if key == "" {
			return nil, nil, nil, http.StatusBadRequest, "correlation keys must not be empty"
		}
		if !correlationKeys[key] {
			return nil, nil, nil, http.StatusBadRequest,
				fmt.Sprintf("correlation key %q is not accepted by system %q", key, request.System)
		}
		correlationBytes += len(key) + len(value)
	}
	if correlationBytes > runcontroller.MaxCorrelationBytes {
		return nil, nil, nil, http.StatusBadRequest,
			fmt.Sprintf("correlation is larger than %d bytes", runcontroller.MaxCorrelationBytes)
	}

	identity := false
	for _, key := range system.Definition.Events.IdentityKeys {
		if _, ok := request.Correlation[key]; ok {
			identity = true
			break
		}
	}
	if !identity {
		return nil, nil, nil, http.StatusBadRequest,
			fmt.Sprintf("correlation must contain at least one identity key of system %q: %s",
				request.System, strings.Join(system.Definition.Events.IdentityKeys, ", "))
	}

	if bindingKey := system.Definition.Events.ProjectBinding; bindingKey != "" {
		if value, ok := request.Correlation[bindingKey]; ok {
			normalized := system.Definition.Normalize(bindingKey, value)
			if !containsString(system.BindingValues, normalized) {
				return nil, nil, nil, http.StatusBadRequest,
					fmt.Sprintf("correlation value %q for project binding %q is not bound to the project",
						value, bindingKey)
			}
		}
	}

	return system, completion, system.Definition.NormalizeCorrelation(request.Correlation), 0, ""
}

// registerSubscription 执行订阅登记的第三步：在可重复读事务里读取计数器的
// next_seq 作为 since_cursor，插入 active 订阅，并按匹配规则回溯
// append_seq < since_cursor 的事件。命中时在同一个事务里完成订阅与投递记录。
func (h *connectorHandler) registerSubscription(
	ctx context.Context,
	sub subscription,
	now time.Time,
	system *System,
) (int64, error) {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, fmt.Errorf("connectorhub: begin subscription transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var sinceCursor int64
	if err := tx.QueryRow(ctx, `SELECT next_seq FROM connector_event_counter`).Scan(&sinceCursor); err != nil {
		return 0, fmt.Errorf("connectorhub: read event counter: %w", err)
	}
	sub.SinceCursor = sinceCursor

	if err := insertSubscription(ctx, tx, sub); err != nil {
		return 0, err
	}
	if _, err := matchSubscription(ctx, tx, sub, 1, sinceCursor, now, system.Connector.Terminal); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("connectorhub: commit subscription: %w", err)
	}
	return sinceCursor, nil
}

// rescanSubscription 执行订阅登记的第四步：在提交后扫描
// append_seq >= since_cursor、本订阅尚未认领的匹配事件。订阅已由回溯完成
// 时不再扫描。
func (h *connectorHandler) rescanSubscription(
	ctx context.Context,
	sub subscription,
	now time.Time,
	system *System,
) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("connectorhub: begin rescan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	current, err := readSubscription(ctx, tx, sub.SubID)
	if err != nil {
		return err
	}
	if current.Status == "active" {
		if _, err := matchSubscription(ctx, tx, current, sub.SinceCursor, maxAppendSeq, now, system.Connector.Terminal); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("connectorhub: commit rescan: %w", err)
	}
	return nil
}

// completeFromVerify 以状态查询结果完成订阅并写入 source 为 verify 的投递
// 记录。订阅已由其他路径完成时条件更新失败，不写任何记录。
func (h *connectorHandler) completeFromVerify(
	ctx context.Context,
	sub subscription,
	terminal string,
	detail []byte,
	at time.Time,
) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("connectorhub: begin verify transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := completeFromStatus(ctx, tx, sub, terminal, detail, at); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("connectorhub: commit verify: %w", err)
	}
	return nil
}

// insertSubscription 插入一条订阅。dedup_spec 以 JSON 保存，供完成事件
// 构造去重键。
func insertSubscription(ctx context.Context, tx pgx.Tx, sub subscription) error {
	correlation, err := encodeCorrelation(sub.Correlation)
	if err != nil {
		return err
	}
	dedupSpec, err := json.Marshal(sub.DedupSpec)
	if err != nil {
		return fmt.Errorf("connectorhub: encode dedup spec: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO subscription
			(sub_id, run_id, project_id, system, event, correlation, status, terminal,
			 since_cursor, dedup_spec, created_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10::jsonb, $11, $12)`,
		sub.SubID, sub.RunID, sub.ProjectID, sub.System, sub.Event, correlation,
		sub.Status, sub.Terminal, sub.SinceCursor, string(dedupSpec), sub.CreatedAt, sub.CompletedAt,
	); err != nil {
		return fmt.Errorf("connectorhub: insert subscription %s: %w", sub.SubID, err)
	}
	return nil
}

// queryRower 是 pgx 连接池与事务共有的 QueryRow 方法。
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readSubscription 按 sub_id 读取一条订阅。
func readSubscription(ctx context.Context, q queryRower, subID string) (subscription, error) {
	sub, err := scanSubscription(q.QueryRow(ctx,
		`SELECT `+subscriptionSelectColumns+` FROM subscription WHERE sub_id = $1`, subID))
	if err != nil {
		return subscription{}, fmt.Errorf("connectorhub: read subscription %s: %w", subID, err)
	}
	return sub, nil
}

// newSubscriptionID 生成 sub- 加 12 位小写 base32 随机串的订阅标识。
func newSubscriptionID() (string, error) {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("connectorhub: generate subscription id: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:])
	return "sub-" + strings.ToLower(encoded)[:subscriptionIDLength], nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
