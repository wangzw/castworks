package connectorhub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// Runs 是 connectorhub 对运行实例服务的依赖，由控制面装配时的
// runcontroller.RunService 实现满足。connectorhub 只用到查询运行实例与
// 写入收件箱两项能力。
type Runs interface {
	Get(ctx context.Context, runID string) (runcontroller.RunDetail, error)
	AppendInbox(ctx context.Context, runID string, ev runcontroller.InboxEvent) (int64, bool, error)
}

// 编译期确认 Runs 满足包内两个窄接口：事件目录与订阅登记只需要 Get，
// 投递与取消扫描需要 Get 与收件箱写入。
var (
	_ runGetter    = Runs(nil)
	_ deliveryRuns = runDelivery{}
)

// Hub 是 connectorhub 对控制面暴露的入口，把 webhook 接收端、connector
// 端点与后台投递扫描组装在一起。
type Hub struct {
	webhook   *webhookHandler
	connector *connectorHandler
	delivery  *deliverer
	logger    *slog.Logger
}

// Option 调整 Hub 的装配参数，只供测试与装配使用。
type Option func(*hubOptions)

type hubOptions struct {
	now           func() time.Time
	deliverPeriod time.Duration
	logger        *slog.Logger
}

// WithClock 注入时钟，测试用它固定事件发生时间与完成时刻。
func WithClock(now func() time.Time) Option {
	return func(o *hubOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// WithDeliverPeriod 注入 P_deliver，即投递重试与取消扫描的周期。
func WithDeliverPeriod(period time.Duration) Option {
	return func(o *hubOptions) {
		o.deliverPeriod = period
	}
}

// WithLogger 注入日志记录器。
func WithLogger(logger *slog.Logger) Option {
	return func(o *hubOptions) {
		if logger != nil {
			o.logger = logger
		}
	}
}

// New 按配置、存储与运行实例服务组装一个 Hub。pool 与 runs 必须非空。
func New(cfg Config, pool *pgxpool.Pool, runs Runs, opts ...Option) (*Hub, error) {
	if pool == nil {
		return nil, errors.New("connectorhub: New requires a database pool")
	}
	if runs == nil {
		return nil, errors.New("connectorhub: New requires a run service")
	}

	options := hubOptions{now: time.Now, logger: slog.Default()}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	webhook := newWebhookHandler(cfg, pool)
	webhook.now = options.now
	webhook.logger = options.logger
	connector := newConnectorHandler(&cfg, pool, runs)
	connector.now = options.now
	delivery := newDeliverer(pool, runDelivery{runs}, options.deliverPeriod, options.now)
	delivery.logger = options.logger

	return &Hub{
		webhook:   webhook,
		connector: connector,
		delivery:  delivery,
		logger:    options.logger,
	}, nil
}

// immediateDeliveryTimeout 是请求结束后立即投递的超时。它独立于请求的 ctx：
// 请求取消不会中断已提交记录的投递；超时后未完成的记录留给后台扫描。
const immediateDeliveryTimeout = 10 * time.Second

// WebhookHandler 返回外部系统 webhook 的接收端。请求处理完成后立即投递
// 本次请求写入的待投递记录；其余记录交给后台扫描按 P_deliver 重试。
func (h *Hub) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collector := &deliveryCollector{}
		h.webhook.ServeHTTP(w, r.WithContext(withDeliveryCollector(r.Context(), collector)))
		h.deliverCollected(collector)
	})
}

// ConnectorHandler 返回面向沙箱的 connector 端点。订阅登记可能提交待投递
// 记录，请求处理完成后立即投递本次请求写入的那些记录。
func (h *Hub) ConnectorHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collector := &deliveryCollector{}
		h.connector.ServeHTTP(w, r.WithContext(withDeliveryCollector(r.Context(), collector)))
		h.deliverCollected(collector)
	})
}

// Run 执行投递重试与取消扫描：启动时扫描一次，之后每 P_deliver 一次，
// ctx 取消后返回 nil。
func (h *Hub) Run(ctx context.Context) error {
	return h.delivery.run(ctx)
}

// deliverCollected 只投递本次请求写入的待投递记录。没有被写入记录的请求，
// 包括 401、404 与 405 的请求，不会触发任何投递。单条记录的暂时性错误由
// 投递器记入 attempts 与 last_error，这里只记录整轮的错误。
func (h *Hub) deliverCollected(collector *deliveryCollector) {
	keys := collector.snapshot()
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), immediateDeliveryTimeout)
	defer cancel()
	if err := h.delivery.deliverRecords(ctx, keys); err != nil && ctx.Err() == nil {
		h.logger.Warn("connectorhub: deliver completions written by the request failed", "error", err)
	}
}

// runDelivery 把 Runs 适配成投递器需要的窄接口。Completion 到
// runcontroller.InboxEvent 的转换只在这里发生。
type runDelivery struct {
	runs Runs
}

// Get 查询运行实例，满足 deliveryRuns 的取消扫描需求。
func (r runDelivery) Get(ctx context.Context, runID string) (runcontroller.RunDetail, error) {
	return r.runs.Get(ctx, runID)
}

// AppendCompletion 把完成事件转换成收件箱事件写入。duplicate 表示收件箱
// 已按 dedup_key 判定为同一事件，与投递成功等价。
func (r runDelivery) AppendCompletion(ctx context.Context, runID string, c Completion) (bool, error) {
	_, duplicate, err := r.runs.AppendInbox(ctx, runID, inboxEventFromCompletion(c))
	return duplicate, err
}

// inboxEventFromCompletion 把订阅完成事件转换成 runcontroller 的收件箱事件。
// 这是包内唯一的转换处；Text 不用于 subscription_completed。
func inboxEventFromCompletion(c Completion) runcontroller.InboxEvent {
	return runcontroller.InboxEvent{
		Kind:     runcontroller.KindSubscriptionCompleted,
		EventID:  c.EventID,
		DedupKey: c.DedupKey,
		Subscription: &runcontroller.SubscriptionCompletion{
			SubID:              c.SubID,
			System:             c.System,
			Event:              c.Event,
			SubscriptionStatus: c.SubscriptionStatus,
			Terminal:           c.Terminal,
			Correlation:        c.Correlation,
			OccurredAt:         c.OccurredAt,
			Source:             c.Source,
			Detail:             c.Detail,
			DetailTruncated:    c.DetailTruncated,
		},
	}
}
