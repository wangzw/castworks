package connectorhub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// completionDetailLimit 是订阅完成事件 detail 字段的字节上限，即 48 KiB。
// 原始内容超过该长度时截断到恰好这么多字节，并把 DetailTruncated 置为真。
const completionDetailLimit = 48 * 1024

// Completion 是待写入收件箱的订阅完成事件。字段与 runcontroller 的
// SubscriptionCompletion 相同，另加 EventID 与 DedupKey；Hub 对外接口把
// 它转换成 runcontroller.InboxEvent，转换只在那里发生。
//
// JSON 字段名与 docs/milestone/v0.2.0/connectorhub.md “订阅完成事件”一节
// 的表格一致，completion_delivery.event 以这个形式保存。
type Completion struct {
	SubID              string            `json:"sub_id"`
	System             string            `json:"system"`
	Event              string            `json:"event"`
	SubscriptionStatus string            `json:"subscription_status"`
	Terminal           string            `json:"terminal"`
	Correlation        map[string]string `json:"correlation"`
	OccurredAt         time.Time         `json:"occurred_at"`
	Source             string            `json:"source"`
	Detail             string            `json:"detail"`
	DetailTruncated    bool              `json:"detail_truncated"`
	EventID            string            `json:"event_id"`
	DedupKey           string            `json:"dedup_key"`
}

// newWebhookCompletion 按 webhook 事件构造完成事件。终态取值由连接器从
// 事件负载取出后传入；occurred_at 取事件的发生时间；detail 取原始负载。
//
// event_id 为 (sub_id, 终态取值) 的 JSON 元组编码；dedup_key 为
// H(sub_id, H(dedup_spec 的字段值))，字段值取自负载，event_type 取事件的
// 规范化类型。
func newWebhookCompletion(sub subscription, ev connectorEvent, terminal string) Completion {
	detail, truncated := truncateDetail(string(ev.Payload))
	return Completion{
		SubID:              sub.SubID,
		System:             sub.System,
		Event:              sub.Event,
		SubscriptionStatus: "completed",
		Terminal:           terminal,
		Correlation:        sub.Correlation,
		OccurredAt:         ev.OccurredAt,
		Source:             "webhook",
		Detail:             detail,
		DetailTruncated:    truncated,
		EventID:            encodeTuple(sub.SubID, terminal),
		DedupKey:           hashTuple(sub.SubID, hashTuple(dedupValues(ev, sub.DedupSpec)...)),
	}
}

// newVerifyCompletion 按状态查询结果构造完成事件。terminal 是查询到的
// 终态取值，detail 是状态查询的响应体，occurredAt 是查询时刻。
//
// event_id 与 webhook 触发相同，为 (sub_id, 终态取值)；dedup_key 为
// H(sub_id, verify, 终态取值)。
func newVerifyCompletion(sub subscription, terminal string, detail []byte, occurredAt time.Time) Completion {
	text, truncated := truncateDetail(string(detail))
	return Completion{
		SubID:              sub.SubID,
		System:             sub.System,
		Event:              sub.Event,
		SubscriptionStatus: "completed",
		Terminal:           terminal,
		Correlation:        sub.Correlation,
		OccurredAt:         occurredAt,
		Source:             "verify",
		Detail:             text,
		DetailTruncated:    truncated,
		EventID:            encodeTuple(sub.SubID, terminal),
		DedupKey:           hashTuple(sub.SubID, "verify", terminal),
	}
}

// truncateDetail 先把 detail 规范化为合法 UTF-8，再截断到 completionDetailLimit
// 字节以内。每一段非法字节替换为一个 U+FFFD（3 字节），替换后的长度计入
// 上限：原始内容带着非法字节时过不了 protobuf 的 string 编码。长度不超过
// 上限时返回规范化结果且不标记截断。截断退到一个 UTF-8 字符的边界，不切开
// 多字节字符；残缺的 UTF-8 既过不了 JSON 往返，也过不了收件箱的字符串编码。
func truncateDetail(detail string) (string, bool) {
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) <= completionDetailLimit {
		return detail, false
	}
	cut := completionDetailLimit
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut], true
}

// casCompleteSubscription 把订阅从 active 条件更新为 completed，并写入
// 终态取值与完成时刻。返回是否更新成功；订阅已由其他路径完成或取消时
// 返回 false，调用方据此不写任何记录。
func casCompleteSubscription(ctx context.Context, tx pgx.Tx, subID, terminal string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE subscription SET status = 'completed', terminal = $2, completed_at = $3
		 WHERE sub_id = $1 AND status = 'active'`,
		subID, terminal, at)
	if err != nil {
		return false, fmt.Errorf("connectorhub: complete subscription %s: %w", subID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// insertCompletionDelivery 写入一条 pending 的完成事件投递记录。
// (sub_id, dedup_key) 唯一；同一完成事件不会产生第二条记录。记录插入成功
// 时同时记入请求的投递收集器，供请求结束后只投递这条记录。
func insertCompletionDelivery(ctx context.Context, tx pgx.Tx, runID string, c Completion, now time.Time) error {
	event, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("connectorhub: encode completion event: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO completion_delivery
			(sub_id, run_id, event_id, dedup_key, event, status, created_at)
		 VALUES ($1, $2, $3, $4, $5::jsonb, 'pending', $6)
		 ON CONFLICT (sub_id, dedup_key) DO NOTHING`,
		c.SubID, runID, c.EventID, c.DedupKey, string(event), now)
	if err != nil {
		return fmt.Errorf("connectorhub: insert completion delivery for %s: %w", c.SubID, err)
	}
	if tag.RowsAffected() > 0 {
		collectDelivery(ctx, deliveryKey{subID: c.SubID, dedupKey: c.DedupKey})
	}
	return nil
}

// deliveryKey 标识 completion_delivery 表中的一行。
type deliveryKey struct {
	subID    string
	dedupKey string
}

// deliveryCollector 收集一次请求写入的投递记录标识，使请求结束后只投递这些
// 记录，其余留给周期扫描。它只记录标识；投递前重新读取记录，因此未提交或
// 已被别处处理的行会被跳过。
type deliveryCollector struct {
	mu   sync.Mutex
	seen map[deliveryKey]struct{}
	keys []deliveryKey
}

// add 记入一条记录标识，重复的标识只保留一次。
func (c *deliveryCollector) add(key deliveryKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return
	}
	if c.seen == nil {
		c.seen = make(map[deliveryKey]struct{})
	}
	c.seen[key] = struct{}{}
	c.keys = append(c.keys, key)
}

// snapshot 返回已收集的记录标识的副本。
func (c *deliveryCollector) snapshot() []deliveryKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]deliveryKey(nil), c.keys...)
}

// deliveryCollectorKey 是请求 ctx 中投递收集器的键。
type deliveryCollectorKey struct{}

// withDeliveryCollector 把一个请求的投递收集器放进 ctx。
func withDeliveryCollector(ctx context.Context, collector *deliveryCollector) context.Context {
	return context.WithValue(ctx, deliveryCollectorKey{}, collector)
}

// collectDelivery 记入一次投递记录的写入。ctx 中没有收集器时什么也不做。
func collectDelivery(ctx context.Context, key deliveryKey) {
	collector, ok := ctx.Value(deliveryCollectorKey{}).(*deliveryCollector)
	if !ok || collector == nil {
		return
	}
	collector.add(key)
}

// dedupValues 按 dedup_spec 的字段路径顺序从事件中取值，缺失的字段取空串。
// event_type 是规范化的事件类型，不是负载中的字段路径；其余路径按点号
// 分隔逐层读取 JSON 对象。
func dedupValues(ev connectorEvent, spec []string) []string {
	values := make([]string, len(spec))
	if len(spec) == 0 {
		return values
	}

	var payload any
	if len(ev.Payload) > 0 {
		dec := json.NewDecoder(bytes.NewReader(ev.Payload))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			payload = nil
		}
	}

	for i, path := range spec {
		if path == "event_type" {
			values[i] = ev.EventType
			continue
		}
		values[i] = scalarString(lookupField(payload, path))
	}
	return values
}

// lookupField 按点号分隔的路径在 JSON 对象中取值。路径不通时返回 nil。
func lookupField(doc any, path string) any {
	current := doc
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[part]
		if !ok {
			return nil
		}
	}
	return current
}

// scalarString 把一个 JSON 值转成去重键里的字符串。字符串原样使用，数字
// 用其字面量，布尔值写作 true 或 false，缺失值写作空串；对象与数组用紧凑
// JSON 文本。
func scalarString(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case json.Number:
		return value.String()
	case bool:
		return strconv.FormatBool(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case map[string]any, []any:
		data, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(data)
	default:
		return fmt.Sprint(value)
	}
}

// encodeTuple 把元组编码为 JSON 数组。元素按给定顺序排列，字符串按 UTF-8
// 编码；控制字符与引号按 encoding/json 的规则转义。
func encodeTuple(parts ...string) string {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, part := range parts {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(encodeJSONString(part))
	}
	buf.WriteByte(']')
	return buf.String()
}

// encodeJSONString 返回 s 的 JSON 字符串字面量。这里关掉 HTML 转义，让
// 非 ASCII 字符与 <、>、& 按原样进入编码。
func encodeJSONString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// 编码字符串不会失败。
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// hashTuple 对元组的 JSON 数组编码求 SHA-256，输出小写十六进制。
func hashTuple(parts ...string) string {
	sum := sha256.Sum256([]byte(encodeTuple(parts...)))
	return hex.EncodeToString(sum[:])
}
