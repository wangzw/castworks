package connectorhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxWebhookBodyBytes 是 webhook 请求体的读取上限。请求体就是事件负载，
// 超过 1 MiB 的投递返回 413，避免无界读取。
const maxWebhookBodyBytes = 1 << 20

// webhookHandler 是外部系统 webhook 的接收端，按路径找到定义与连接器后
// 校验签名与投递标识，写入连接器事件日志并匹配活动订阅。Hub 对外接口把
// 它接到 WebhookHandler。
type webhookHandler struct {
	config Config
	pool   *pgxpool.Pool

	// now 提供请求的处理时刻，测试可替换。
	now func() time.Time
	// matchEvent 在写事务提交之后匹配活动订阅，测试可替换以注入数据库错误。
	matchEvent func(ctx context.Context, tx pgx.Tx, ev connectorEvent, terminal string, now time.Time) (int, error)
	// logger 记录被丢弃的未知事件类型。每条这样的请求只写一行。
	logger *slog.Logger
}

// newWebhookHandler 构造 webhook 接收端。
func newWebhookHandler(config Config, pool *pgxpool.Pool) *webhookHandler {
	return &webhookHandler{
		config:     config,
		pool:       pool,
		now:        time.Now,
		matchEvent: matchEvent,
		logger:     slog.Default(),
	}
}

// ServeHTTP 按 connectorhub.md“webhook 摄入”一节的六步处理一次投递。
func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 1. 按路径找到定义与连接器。
	system, ok := h.config.SystemByWebhookPath(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	source, ok := webhookSourceForPath(system.Definition, r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// 2. 读取签名密钥、请求体，校验签名，读取投递标识，解析 JSON。
	key, err := os.ReadFile(system.SigningKeyFile)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	key = trimSigningKey(key)
	if len(key) == 0 {
		// 空密钥会让校验退化成对公开值的比较，按密钥不可读处理。
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body is larger than 1 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	if !system.Connector.VerifySignature(r.Header, body, key) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get(source.DeliveryID))
	if deliveryID == "" {
		http.Error(w, "missing delivery id", http.StatusBadRequest)
		return
	}
	payload, err := decodeWebhookJSON(body)
	if err != nil {
		http.Error(w, "request body is not JSON", http.StatusBadRequest)
		return
	}

	// 3. 由连接器取得事件类型。定义未声明的事件类型返回 200 且不写入，
	// 记录一行日志；发送方收不到 2xx 会一直重试，未知类型不是它的错误。
	eventType, err := system.Connector.EventType(r.Header)
	if err != nil {
		http.Error(w, "cannot determine event type", http.StatusBadRequest)
		return
	}
	if !definitionProducesEvent(system.Definition, eventType) {
		h.logger.Warn("connectorhub: dropping webhook with an event type outside the definition",
			"system", system.Name, "path", r.URL.Path, "event_type", eventType,
			"delivery_id", deliveryID)
		w.WriteHeader(http.StatusOK)
		return
	}

	occurredAt, err := extractOccurredAt(payload, system.Definition.Events.Ordering.OccurredAt)
	if err != nil {
		http.Error(w, "request body has no valid occurred_at", http.StatusBadRequest)
		return
	}
	correlation := extractCorrelation(system.Definition, payload)

	var terminal string
	if _, ok := system.Definition.CompletionFor(eventType); ok {
		terminal, err = system.Connector.Terminal(body)
		if err != nil || terminal == "" {
			http.Error(w, "completion event has no terminal value", http.StatusBadRequest)
			return
		}
	}

	// 项目归属键未绑定的事件照常写入，project_ids 为空，不参与匹配。
	projectIDs := h.config.ProjectsForBinding(system.Name, correlation[system.Definition.Events.ProjectBinding])
	if projectIDs == nil {
		projectIDs = []string{}
	}

	ev := connectorEvent{
		System:      system.Name,
		DeliveryID:  deliveryID,
		EventType:   eventType,
		ProjectIDs:  projectIDs,
		Correlation: correlation,
		OccurredAt:  occurredAt,
		Payload:     body,
	}
	now := h.now()

	// 4. 在一个事务中分配 append_seq 并写入事件；投递标识冲突时不写入。
	stored, duplicate, err := h.writeEvent(r.Context(), ev, now)
	if err != nil {
		http.Error(w, "cannot record webhook event", http.StatusInternalServerError)
		return
	}

	// 5. 写入事务已提交。

	// 6. 匹配活动订阅。投递标识已存在时同样执行这一步，用日志中保存的
	// 原始事件重新匹配；第一次匹配失败后发送方重试时由它补上。
	if duplicate {
		ev = stored
	}
	if _, ok := system.Definition.CompletionFor(ev.EventType); ok {
		if duplicate {
			terminal, err = system.Connector.Terminal(ev.Payload)
			if err != nil || terminal == "" {
				http.Error(w, "completion event has no terminal value", http.StatusBadRequest)
				return
			}
		}
		if err := h.match(r.Context(), ev, terminal, now); err != nil {
			http.Error(w, "cannot match subscriptions", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// writeEvent 在事务中分配追加序号并写入连接器事件。事务先锁住计数器的
// 唯一一行，分配、写入与提交都在持锁期间完成，因此 append_seq 的分配
// 顺序就是提交顺序。投递标识已存在时不写入，返回日志中的事件与 true。
func (h *webhookHandler) writeEvent(ctx context.Context, ev connectorEvent, now time.Time) (connectorEvent, bool, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: begin webhook transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var nextSeq int64
	if err := tx.QueryRow(ctx,
		`SELECT next_seq FROM connector_event_counter WHERE id FOR UPDATE`,
	).Scan(&nextSeq); err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: lock event counter: %w", err)
	}

	var duplicate bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM connector_event WHERE system = $1 AND delivery_id = $2)`,
		ev.System, ev.DeliveryID,
	).Scan(&duplicate); err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: check delivery %s: %w", ev.DeliveryID, err)
	}
	if duplicate {
		stored, err := scanWebhookEvent(tx.QueryRow(ctx,
			`SELECT append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload
			   FROM connector_event WHERE system = $1 AND delivery_id = $2`,
			ev.System, ev.DeliveryID,
		))
		if err != nil {
			return connectorEvent{}, false, fmt.Errorf("connectorhub: read existing delivery %s: %w", ev.DeliveryID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return connectorEvent{}, false, fmt.Errorf("connectorhub: commit duplicate check: %w", err)
		}
		return stored, true, nil
	}

	ev.AppendSeq = nextSeq
	if _, err := tx.Exec(ctx,
		`UPDATE connector_event_counter SET next_seq = next_seq + 1 WHERE id`,
	); err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: advance event counter: %w", err)
	}

	correlation, err := json.Marshal(ev.Correlation)
	if err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: encode event correlation: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO connector_event
			(append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload, received_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)`,
		ev.AppendSeq, ev.System, ev.DeliveryID, ev.EventType, ev.ProjectIDs,
		string(correlation), ev.OccurredAt, ev.Payload, now); err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: insert event %s: %w", ev.DeliveryID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return connectorEvent{}, false, fmt.Errorf("connectorhub: commit event %s: %w", ev.DeliveryID, err)
	}
	return ev, false, nil
}

// match 在独立事务中把一条事件与全部活动订阅匹配。
func (h *webhookHandler) match(ctx context.Context, ev connectorEvent, terminal string, now time.Time) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("connectorhub: begin match transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := h.matchEvent(ctx, tx, ev, terminal, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("connectorhub: commit match transaction: %w", err)
	}
	return nil
}

// scanWebhookEvent 读取 connector_event 的一行。
func scanWebhookEvent(row rowScanner) (connectorEvent, error) {
	var ev connectorEvent
	if err := row.Scan(
		&ev.AppendSeq, &ev.System, &ev.DeliveryID, &ev.EventType,
		&ev.ProjectIDs, &ev.Correlation, &ev.OccurredAt, &ev.Payload,
	); err != nil {
		return connectorEvent{}, err
	}
	return ev, nil
}

// webhookSourceForPath 返回定义中路径匹配的 webhook 事件源。
func webhookSourceForPath(definition Definition, path string) (EventSource, bool) {
	for _, source := range definition.Events.Sources {
		if source.Type == "webhook" && source.Path == path {
			return source, true
		}
	}
	return EventSource{}, false
}

// definitionProducesEvent 判断事件类型是否由定义的 webhook 事件源声明。
func definitionProducesEvent(definition Definition, eventType string) bool {
	for _, source := range definition.Events.Sources {
		if source.Type != "webhook" {
			continue
		}
		for _, produced := range source.EventTypes {
			if produced == eventType {
				return true
			}
		}
	}
	return false
}

// trimSigningKey 去掉签名密钥文件末尾的一个 "\r\n" 或 "\n"，其余字节原样使用。
// 只去掉一个换行：以 "\r\n" 结尾时去它，否则以 "\n" 结尾时去 "\n"。
func trimSigningKey(key []byte) []byte {
	if bytes.HasSuffix(key, []byte("\r\n")) {
		return key[:len(key)-2]
	}
	if bytes.HasSuffix(key, []byte("\n")) {
		return key[:len(key)-1]
	}
	return key
}

// decodeWebhookJSON 解析请求体，要求恰好一个 JSON 值。数字保留字面量，
// 供关联键与去重键取值。
func decodeWebhookJSON(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, err
	}
	return value, nil
}

// extractOccurredAt 按定义中的字段路径取事件发生时间，按 RFC 3339 解析。
func extractOccurredAt(payload any, path string) (time.Time, error) {
	text := scalarString(lookupField(payload, path))
	if text == "" {
		return time.Time{}, fmt.Errorf("no value at %q", path)
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %q as time: %w", path, err)
	}
	return occurredAt, nil
}

// extractCorrelation 按 correlation_keys 的字段路径从负载取出关联键，并按
// 定义规范化。缺失的字段取空串。
func extractCorrelation(definition Definition, payload any) map[string]string {
	events := definition.Events
	correlation := make(map[string]string, len(events.CorrelationKeys))
	for _, key := range events.CorrelationKeys {
		correlation[key] = definition.Normalize(key, scalarString(lookupField(payload, key)))
	}
	return correlation
}
