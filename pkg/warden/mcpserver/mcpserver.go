// Package mcpserver 提供看守进程的平台 MCP 服务器。
//
// 对外接口：
//
//	func Start(cfg Config) (*Server, error)
//	func (s *Server) URL() string
//	func (s *Server) Close() error
//
// Start 在 cfg.Addr 上监听，以 Streamable HTTP 传输在路径 /mcp 提供名为
// castworks 的服务器，只包含 list_events 与 subscribe_event 两个工具。
// 两个工具以 cfg.RunID 标识运行实例调用 connector 端点，单次调用超时 10 s。
// URL 返回 Agent 连接用的地址；Close 关闭监听与在途连接，可重复调用。
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "castworks"
	serverVersion = "0.2.0"
	path          = "/mcp"

	// toolTimeout 是单个工具调用访问 connector 端点的超时。
	toolTimeout = 10 * time.Second

	// retryMessage 用于 5xx、超时与连接失败：平台暂时不可用，可以重试。
	retryMessage = "The platform is temporarily unavailable; you can retry this call later."
)

// waitRules 是设计“等待方式的选择”中适用于 v0.2.0 两个工具的条目，
// 去掉与 wait_for、sleep_until 有关的部分；用英文写给 Agent。
// instructions 与两个工具的描述各自完整承载这组规则。
const waitRules = `How to wait for an external system:
- Call list_events first to see which events are available for this run.
- To wait for an external system, call subscribe_event and end your turn. The event is delivered as input to your next turn, and the run can be suspended while it waits.
- If you need to keep working, you may register a subscription and continue the current turn; the event is delivered to a later turn.
- When you need human input, end your turn. The human reply arrives as input to your next turn.
- Do not poll external systems with Bash, curl or other external tools: polling prevents the run from being suspended and duplicates a subscription.
- In-session timers, Monitor and background Bash remain available.
- If you can estimate how long you will wait, or want to state why, fill in expected_wait (a duration such as 30s, 5m or 2h) and reason (a short explanation). The call works the same without them.`

const instructions = `This server provides two tools for waiting on external systems.

` + waitRules

const listEventsDescription = `List the external-system events available for this run. This tool is read-only and never blocks.

` + waitRules

const subscribeEventDescription = `Register a subscription to an external-system event and return its subscription ID.

` + waitRules

// Config 配置平台 MCP 服务器。
type Config struct {
	// RunID 是运行实例标识，写在 connector 端点的路径上。
	RunID string
	// ConnectorAddr 是 connector 端点的地址，取 host:port 或带 scheme 的 URL。
	ConnectorAddr string
	// Addr 是监听地址，默认 127.0.0.1:0（由系统分配端口）。
	Addr string
	// Logger 接收日志，默认 slog.Default()。
	Logger *slog.Logger
}

// Server 是运行中的平台 MCP 服务器。
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	done       chan struct{}
	log        *slog.Logger
	runID      string
	connector  *url.URL
	timeout    time.Duration
	calls      atomic.Int64
}

// Start 启动平台 MCP 服务器并返回；监听失败时返回错误。
func Start(cfg Config) (*Server, error) {
	return start(cfg, toolTimeout)
}

func start(cfg Config, timeout time.Duration) (*Server, error) {
	if cfg.RunID == "" {
		return nil, errors.New("mcpserver: run ID is required")
	}
	if cfg.ConnectorAddr == "" {
		return nil, errors.New("mcpserver: connector address is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	connector, err := url.Parse(connectorBase(cfg.ConnectorAddr))
	if err != nil || connector.Host == "" || (connector.Scheme != "http" && connector.Scheme != "https") {
		return nil, fmt.Errorf("mcpserver: invalid connector address %q", cfg.ConnectorAddr)
	}

	s := &Server{
		done:      make(chan struct{}),
		log:       logger,
		runID:     cfg.RunID,
		connector: connector,
		timeout:   timeout,
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_events", Description: listEventsDescription}, s.listEvents)
	mcp.AddTool(srv, &mcp.Tool{Name: "subscribe_event", Description: subscribeEventDescription}, s.subscribeEvent)

	mux := http.NewServeMux()
	mux.Handle(path, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: listen %s: %w", addr, err)
	}
	s.listener = listener
	s.httpServer = &http.Server{Handler: mux}
	go func() {
		defer close(s.done)
		// Serve 在 Close 后返回 ErrServerClosed；其余错误说明监听不可用。
		_ = s.httpServer.Serve(listener)
	}()

	return s, nil
}

// URL 返回 Agent 连接用的 MCP 端点地址。
func (s *Server) URL() string {
	addr, ok := s.listener.Addr().(*net.TCPAddr)
	if !ok {
		return "http://" + s.listener.Addr().String() + path
	}
	ip := addr.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(addr.Port)) + path
}

// Close 关闭监听与在途连接，等待服务循环退出。
func (s *Server) Close() error {
	err := s.httpServer.Close()
	<-s.done
	return err
}

type listEventsInput struct {
	System string `json:"system,omitempty" jsonschema:"external system name; omit to list events from every system"`
}

type subscribeEventInput struct {
	System       string            `json:"system" jsonschema:"external system name"`
	Event        string            `json:"event" jsonschema:"event type to wait for"`
	Correlation  map[string]string `json:"correlation" jsonschema:"correlation keys and values identifying the external object; keys and values are strings"`
	ExpectedWait string            `json:"expected_wait,omitempty" jsonschema:"optional estimated wait duration, for example 30s, 5m or 2h"`
	Reason       string            `json:"reason,omitempty" jsonschema:"optional short explanation of why you are waiting"`
}

func (s *Server) listEvents(ctx context.Context, _ *mcp.CallToolRequest, in listEventsInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	endpoint, err := s.runURL("events")
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "list_events", "error", err)
		return errorResult(err.Error()), nil, nil
	}
	if in.System != "" {
		q := endpoint.Query()
		q.Set("system", in.System)
		endpoint.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "list_events", "error", err)
		return errorResult(retryMessage), nil, nil
	}

	resp, body, err := s.do(req)
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "list_events", "error", err)
		return errorResult(retryMessage), nil, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return errorResult(endpointError(resp.StatusCode, body)), nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.log.Warn("connector endpoint returned an unexpected status", "run_id", s.runID, "tool", "list_events", "status", resp.StatusCode)
		return errorResult(retryMessage), nil, nil
	}
	// EventDescriptor 不经看守进程解析，结果就是响应体原文。
	return textResult(string(body)), nil, nil
}

func (s *Server) subscribeEvent(ctx context.Context, _ *mcp.CallToolRequest, in subscribeEventInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	callID := fmt.Sprintf("call-%d", s.calls.Add(1))
	if in.ExpectedWait != "" || in.Reason != "" {
		s.log.Info("platform MCP tool call",
			"run_id", s.runID,
			"tool", "subscribe_event",
			"call_id", callID,
			"expected_wait", in.ExpectedWait,
			"reason", in.Reason)
	}

	// expected_wait 与 reason 不转发，只进日志。
	payload := struct {
		System      string            `json:"system"`
		Event       string            `json:"event"`
		Correlation map[string]string `json:"correlation"`
	}{System: in.System, Event: in.Event, Correlation: in.Correlation}
	data, err := json.Marshal(payload)
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "error", err)
		return errorResult(retryMessage), nil, nil
	}

	endpoint, err := s.runURL("subscriptions")
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "error", err)
		return errorResult(err.Error()), nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "error", err)
		return errorResult(retryMessage), nil, nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, body, err := s.do(req)
	if err != nil {
		s.log.Warn("platform MCP tool call failed", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "error", err)
		return errorResult(retryMessage), nil, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return errorResult(endpointError(resp.StatusCode, body)), nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.log.Warn("connector endpoint returned an unexpected status", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "status", resp.StatusCode)
		return errorResult(retryMessage), nil, nil
	}

	var out struct {
		SubID  string `json:"sub_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.SubID == "" {
		s.log.Warn("connector endpoint returned an invalid subscription", "run_id", s.runID, "tool", "subscribe_event", "call_id", callID, "error", err)
		return errorResult(retryMessage), nil, nil
	}
	return textResult(subscriptionText(out.SubID, out.Status)), nil, nil
}

// do 发出请求并读完响应体；调用方负责判断状态码。
func (s *Server) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func connectorBase(addr string) string {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// runURL 返回 connector 端点下本次运行实例的 resource URL。run_id 用
// url.PathEscape 转义后作为单个路径段拼接，其中的 "/" 不会被拆成多段。
// PathEscape 不转义点号，"." 与 ".." 仍会被路径规范化折叠，因此这两个值
// 直接拒绝，不发出请求。
func (s *Server) runURL(resource string) (*url.URL, error) {
	if s.runID == "." || s.runID == ".." {
		return nil, fmt.Errorf("run_id %q is invalid: \".\" and \"..\" cannot be used as a run ID", s.runID)
	}
	basePath := strings.TrimRight(s.connector.Path, "/")
	baseEscaped := strings.TrimRight(s.connector.EscapedPath(), "/")
	u := *s.connector
	u.Path = basePath + "/v1/runs/" + s.runID + "/" + resource
	u.RawPath = baseEscaped + "/v1/runs/" + url.PathEscape(s.runID) + "/" + resource
	return &u, nil
}

// endpointError 取 4xx 响应里的 error 字段，作为面向 Agent 的错误内容。
func endpointError(status int, body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		return text
	}
	return fmt.Sprintf("connector endpoint returned status %d", status)
}

func subscriptionText(subID, status string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sub_id: %s\nstatus: %s\n", subID, status)
	if status == "completed" {
		b.WriteString("The event has already arrived and was written to your inbox; it will be delivered as input to your next turn. You can end this turn now.\n")
	} else {
		b.WriteString("The event will be delivered as input to your next turn; you can end this turn now. If you still have work to do, you may continue this turn and handle the event later.\n")
	}
	return b.String()
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}
