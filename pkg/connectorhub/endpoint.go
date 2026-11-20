package connectorhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// EventDescriptor 是事件目录中的一条事件，字段与取值规则见设计文档“对平台
// MCP 服务器的事件目录接口”一节。JSON 字段名取设计字段名的 snake_case。
//
// v0.2.0 只有 once 订阅，因此 Lifetime 恒为 ["once"]；Terminal 取完成条件
// 的 verify.terminal，没有 verify 时为空，Verify 为假。
type EventDescriptor struct {
	System          string   `json:"system"`
	Event           string   `json:"event"`
	Description     string   `json:"description"`
	CorrelationKeys []string `json:"correlation_keys"`
	IdentityKeys    []string `json:"identity_keys"`
	Terminal        []string `json:"terminal"`
	Lifetime        []string `json:"lifetime"`
	Verify          bool     `json:"verify"`
}

// runGetter 是 connectorhub 对运行实例服务的包内窄接口。事件目录与订阅
// 登记只需要确认运行实例存在、没有结束，并取得它的状态，因此这里只声明
// Get；Hub 对外接口工作项再把更宽的 Runs 接口接到它上面。
type runGetter interface {
	Get(ctx context.Context, runID string) (runcontroller.RunDetail, error)
}

// connectorHandler 实现面向沙箱的 connector 端点上的两个路由：事件目录与
// 订阅登记。它由 Hub 对外接口工作项接到 Hub.ConnectorHandler 上。
type connectorHandler struct {
	config *Config
	pool   *pgxpool.Pool
	runs   runGetter

	// now 返回当前时刻，测试可注入固定时钟。
	now func() time.Time
	// afterSnapshot 在订阅登记的快照事务提交之后、重扫之前调用，测试用它
	// 把一条事件插进快照与重扫之间。
	afterSnapshot func()
}

// newConnectorHandler 构造 connector 端点的处理器。runs 提供运行实例查询，
// pool 提供连接器事件日志与订阅的存储。
func newConnectorHandler(config *Config, pool *pgxpool.Pool, runs runGetter) *connectorHandler {
	return &connectorHandler{
		config: config,
		pool:   pool,
		runs:   runs,
		now:    time.Now,
	}
}

// ServeHTTP 路由 GET /v1/runs/{run_id}/events 与
// POST /v1/runs/{run_id}/subscriptions，其余路径返回 404。
func (h *connectorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/runs/")
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	runID, action, ok := strings.Cut(rest, "/")
	if !ok || runID == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	switch action {
	case "events":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.handleEvents(w, r, runID)
	case "subscriptions":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.handleSubscribe(w, r, runID)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// handleEvents 返回运行实例可用外部系统的事件目录。system 为空时返回全部
// 外部系统；给出未装载的外部系统名时返回 400。
func (h *connectorHandler) handleEvents(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()
	if status, message := h.checkRun(ctx, runID); status != 0 {
		writeError(w, status, message)
		return
	}

	systemName := r.URL.Query().Get("system")
	systems := h.config.Systems
	if systemName != "" {
		system, ok := h.config.System(systemName)
		if !ok {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("external system %q is not available", systemName))
			return
		}
		systems = []System{*system}
	}

	descriptors := make([]EventDescriptor, 0, len(systems))
	for i := range systems {
		descriptors = append(descriptors, catalogueFor(&systems[i])...)
	}
	writeJSON(w, http.StatusOK, descriptors)
}

// catalogueFor 由外部系统定义生成事件目录。v0.2.0 只有有完成条件的事件可
// 订阅，因此按 events.completions 投影；同一外部系统内按事件类型字典序。
func catalogueFor(system *System) []EventDescriptor {
	completions := append([]CompletionCondition(nil), system.Definition.Events.Completions...)
	sort.Slice(completions, func(i, j int) bool {
		return completions[i].Event < completions[j].Event
	})

	descriptors := make([]EventDescriptor, 0, len(completions))
	for i := range completions {
		completion := &completions[i]
		terminal := []string{}
		verify := false
		if completion.Verify != nil {
			terminal = append(terminal, completion.Verify.Terminal...)
			verify = true
		}
		descriptors = append(descriptors, EventDescriptor{
			System:          system.Name,
			Event:           completion.Event,
			Description:     describeEvent(system.Name, completion.Event, terminal),
			CorrelationKeys: append([]string{}, system.Definition.Events.CorrelationKeys...),
			IdentityKeys:    append([]string{}, system.Definition.Events.IdentityKeys...),
			Terminal:        terminal,
			Lifetime:        []string{"once"},
			Verify:          verify,
		})
	}
	return descriptors
}

// describeEvent 生成面向 Agent 的事件说明：事件来自哪个外部系统、何时产生、
// 哪些终态取值算完成。说明是英文，端点整体面向 Agent。
func describeEvent(system, event string, terminal []string) string {
	description := fmt.Sprintf("%s event from external system %s", event, system)
	if len(terminal) > 0 {
		return description + "; completes when the terminal value is one of: " +
			strings.Join(terminal, ", ")
	}
	return description
}

// checkRun 确认运行实例存在且未结束。运行实例不存在返回 404，已结束或已
// 失败返回 409；返回的 status 为 0 表示可以继续。
func (h *connectorHandler) checkRun(ctx context.Context, runID string) (int, string) {
	detail, err := h.runs.Get(ctx, runID)
	switch {
	case errors.Is(err, runcontroller.ErrNotFound):
		return http.StatusNotFound, "run not found"
	case errors.Is(err, runcontroller.ErrTerminated):
		return http.StatusConflict, "run has ended or failed"
	case err != nil:
		return http.StatusInternalServerError, "internal error"
	}
	if detail.State == runcontroller.StateEnded || detail.State == runcontroller.StateFailed {
		return http.StatusConflict, "run has ended or failed"
	}
	return 0, ""
}

// writeJSON 写一个 JSON 响应。端点上的成功响应与错误响应都是 JSON。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError 按接口清单写统一的错误响应体 {"error": "..."}。
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
