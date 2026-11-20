package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testRunID = "run-1"

func TestInitializeServesInstructionsAndTools(t *testing.T) {
	t.Parallel()

	srv := startTestServer(t, "127.0.0.1:1", nil, 0)
	session := connectTestClient(t, srv)

	init := session.InitializeResult()
	if init.ServerInfo == nil || init.ServerInfo.Name != "castworks" {
		t.Fatalf("server name = %+v, want castworks", init.ServerInfo)
	}
	for _, want := range []string{"list_events", "subscribe_event", "Do not poll", "expected_wait", "end your turn"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("instructions do not mention %q:\n%s", want, init.Instructions)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		names = append(names, tool.Name)
		// 每个工具描述都独立承载完整的等待规则，不依赖 instructions。
		for _, want := range []string{"Do not poll", "subscribe_event", "expected_wait"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("tool %q description does not mention %q:\n%s", tool.Name, want, tool.Description)
			}
		}
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "list_events,subscribe_event"; got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
}

func TestRulesExcludeWaitForAndSleepUntil(t *testing.T) {
	t.Parallel()

	srv := startTestServer(t, "127.0.0.1:1", nil, 0)
	session := connectTestClient(t, srv)

	texts := map[string]string{"instructions": session.InitializeResult().Instructions}
	for name, tool := range listTools(t, session) {
		texts["tool "+name] = tool.Description
	}
	for where, text := range texts {
		for _, banned := range []string{"wait_for", "sleep_until"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s mentions %q, which v0.2.0 does not provide:\n%s", where, banned, text)
			}
		}
	}
}

type decodedToolSchema struct {
	Type       string `json:"type"`
	Properties map[string]struct {
		Type                 string          `json:"type"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	} `json:"properties"`
	Required []string `json:"required"`
}

func TestToolInputSchemas(t *testing.T) {
	t.Parallel()

	srv := startTestServer(t, "127.0.0.1:1", nil, 0)
	session := connectTestClient(t, srv)
	tools := listTools(t, session)

	listEvents, ok := tools["list_events"]
	if !ok {
		t.Fatal("list_events tool is missing")
	}
	listSchema := decodeToolSchema(t, listEvents.InputSchema)
	if listSchema.Type != "object" {
		t.Errorf("list_events schema type = %q, want object", listSchema.Type)
	}
	if system, ok := listSchema.Properties["system"]; !ok || system.Type != "string" {
		t.Errorf("list_events system = %+v (present %v), want an optional string", system, ok)
	}
	if containsString(listSchema.Required, "system") {
		t.Errorf("list_events required = %v, want system optional", listSchema.Required)
	}

	subscribeEvent, ok := tools["subscribe_event"]
	if !ok {
		t.Fatal("subscribe_event tool is missing")
	}
	schema := decodeToolSchema(t, subscribeEvent.InputSchema)
	if schema.Type != "object" {
		t.Errorf("subscribe_event schema type = %q, want object", schema.Type)
	}

	correlation, ok := schema.Properties["correlation"]
	if !ok {
		t.Fatal("subscribe_event schema has no correlation property")
	}
	if correlation.Type != "object" {
		t.Errorf("correlation type = %q, want object", correlation.Type)
	}
	var additional struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(correlation.AdditionalProperties, &additional); err != nil || additional.Type != "string" {
		t.Errorf("correlation additionalProperties = %s, want a string-to-string object", correlation.AdditionalProperties)
	}

	for _, name := range []string{"expected_wait", "reason"} {
		prop, ok := schema.Properties[name]
		if !ok || prop.Type != "string" {
			t.Errorf("subscribe_event %s = %+v (present %v), want an optional string", name, prop, ok)
		}
		if containsString(schema.Required, name) {
			t.Errorf("subscribe_event required = %v, want %s optional", schema.Required, name)
		}
	}
	for _, name := range []string{"system", "event", "correlation"} {
		if !containsString(schema.Required, name) {
			t.Errorf("subscribe_event required = %v, want %s required", schema.Required, name)
		}
	}
}

func decodeToolSchema(t *testing.T, schema any) decodedToolSchema {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var out decodedToolSchema
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode input schema %s: %v", raw, err)
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func listTools(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools := make(map[string]*mcp.Tool)
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		tools[tool.Name] = tool
	}
	return tools
}

func TestListEventsForwardsAndReturnsRawBody(t *testing.T) {
	t.Parallel()

	const rawBody = `[{"event_type":"job.completed","note":"spacing  preserved"}]`
	stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rawBody)
	})
	srv := startTestServer(t, stub.URL(), nil, 0)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "list_events", map[string]any{"system": "extsim"})
	if res.IsError {
		t.Fatalf("list_events returned an error: %s", resultText(t, res))
	}
	if got := resultText(t, res); got != rawBody {
		t.Fatalf("list_events result = %q, want the raw response body %q", got, rawBody)
	}

	req := stub.lastRequest(t)
	if req.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", req.Method)
	}
	if req.Path != "/v1/runs/"+testRunID+"/events" {
		t.Errorf("path = %q", req.Path)
	}
	if got := req.Query.Get("system"); got != "extsim" {
		t.Errorf("system query = %q, want extsim", got)
	}
	if len(req.Body) != 0 {
		t.Errorf("GET body = %q, want empty", req.Body)
	}
}

func TestListEventsOmitsEmptySystem(t *testing.T) {
	t.Parallel()

	stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	srv := startTestServer(t, stub.URL(), nil, 0)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "list_events", nil)
	if res.IsError {
		t.Fatalf("list_events returned an error: %s", resultText(t, res))
	}
	req := stub.lastRequest(t)
	if _, ok := req.Query["system"]; ok {
		t.Errorf("system query = %q, want it omitted", req.Query.Get("system"))
	}
}

func TestToolPathsEscapeRunIDAsSingleSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		runID string
	}{
		{name: "slash stays inside one segment", runID: "run/1"},
		{name: "dot dot stays inside one segment", runID: "run/../1"},
	}
	calls := []struct {
		tool   string
		args   map[string]any
		suffix string
	}{
		{tool: "list_events", suffix: "events"},
		{
			tool:   "subscribe_event",
			args:   map[string]any{"system": "extsim", "event": "job.completed", "correlation": map[string]string{"job.id": "job-42"}},
			suffix: "subscriptions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"sub_id":"sub-1","status":"active"}`)
			})
			srv := startTestServerWithRunID(t, tt.runID, stub.URL(), nil, 0)
			session := connectTestClient(t, srv)

			for _, call := range calls {
				res := callTool(t, session, call.tool, call.args)
				if res.IsError {
					t.Fatalf("%s returned an error: %s", call.tool, resultText(t, res))
				}
				want := "/v1/runs/" + url.PathEscape(tt.runID) + "/" + call.suffix
				if got := stub.lastRequest(t).EscapedPath; got != want {
					t.Errorf("%s connector path = %q, want the run ID escaped as one segment %q", call.tool, got, want)
				}
			}
		})
	}
}

// TestToolRejectsDotRunIDs 验证 run_id 为 "." 或 ".." 时两个工具都直接
// 返回错误，且不向 connector 发出任何请求。
func TestToolRejectsDotRunIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		runID string
	}{
		{name: "single dot", runID: "."},
		{name: "double dot", runID: ".."},
	}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{tool: "list_events"},
		{
			tool: "subscribe_event",
			args: map[string]any{"system": "extsim", "event": "job.completed", "correlation": map[string]string{"job.id": "job-42"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"sub_id":"sub-1","status":"active"}`)
			})
			srv := startTestServerWithRunID(t, tt.runID, stub.URL(), nil, 0)
			session := connectTestClient(t, srv)

			for _, call := range calls {
				res := callTool(t, session, call.tool, call.args)
				if !res.IsError {
					t.Fatalf("%s with run ID %q was not rejected: %s", call.tool, tt.runID, resultText(t, res))
				}
				if text := resultText(t, res); !strings.Contains(text, "run_id") {
					t.Errorf("%s error does not mention run_id: %q", call.tool, text)
				}
			}
			if got := stub.requestCount(); got != 0 {
				t.Fatalf("connector received %d request(s) for run ID %q, want none", got, tt.runID)
			}
		})
	}
}

func TestSubscribeEventForwardsOnlySubscriptionFields(t *testing.T) {
	t.Parallel()

	stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sub_id":"sub-1","since_cursor":7,"status":"active"}`)
	})
	srv := startTestServer(t, stub.URL(), nil, 0)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "subscribe_event", map[string]any{
		"system":        "extsim",
		"event":         "job.completed",
		"correlation":   map[string]string{"job.id": "job-42"},
		"expected_wait": "5m",
		"reason":        "waiting for the build to finish",
	})
	if res.IsError {
		t.Fatalf("subscribe_event returned an error: %s", resultText(t, res))
	}
	text := resultText(t, res)
	for _, want := range []string{"sub-1", "active", "next turn", "end this turn now"} {
		if !strings.Contains(text, want) {
			t.Errorf("result does not mention %q:\n%s", want, text)
		}
	}

	req := stub.lastRequest(t)
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Path != "/v1/runs/"+testRunID+"/subscriptions" {
		t.Errorf("path = %q", req.Path)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode forwarded body %q: %v", req.Body, err)
	}
	wantKeys := []string{"correlation", "event", "system"}
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != strings.Join(wantKeys, ",") {
		t.Fatalf("forwarded body keys = %s, want %s (body %s)", got, strings.Join(wantKeys, ","), req.Body)
	}
	var forwarded struct {
		System      string            `json:"system"`
		Event       string            `json:"event"`
		Correlation map[string]string `json:"correlation"`
	}
	if err := json.Unmarshal(req.Body, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if forwarded.System != "extsim" || forwarded.Event != "job.completed" {
		t.Errorf("forwarded system/event = %q/%q", forwarded.System, forwarded.Event)
	}
	if got := forwarded.Correlation["job.id"]; got != "job-42" {
		t.Errorf("forwarded correlation[job.id] = %q, want job-42", got)
	}
}

func TestSubscribeEventCompletedStatus(t *testing.T) {
	t.Parallel()

	stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sub_id":"sub-2","since_cursor":3,"status":"completed"}`)
	})
	srv := startTestServer(t, stub.URL(), nil, 0)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "subscribe_event", map[string]any{
		"system":      "extsim",
		"event":       "job.completed",
		"correlation": map[string]string{"job.id": "job-42"},
	})
	if res.IsError {
		t.Fatalf("subscribe_event returned an error: %s", resultText(t, res))
	}
	text := resultText(t, res)
	for _, want := range []string{"sub-2", "completed", "inbox", "next turn", "end this turn now"} {
		if !strings.Contains(text, want) {
			t.Errorf("result does not mention %q:\n%s", want, text)
		}
	}
}

func TestSubscribeEventLogsWaitIntent(t *testing.T) {
	t.Parallel()

	stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sub_id":"sub-3","status":"active"}`)
	})
	recorder := &logRecorder{}
	logger := slog.New(recorder)
	srv := startTestServer(t, stub.URL(), logger, 0)
	session := connectTestClient(t, srv)

	const (
		expectedWait = "90s"
		reason       = "waiting for an external approval"
	)
	res := callTool(t, session, "subscribe_event", map[string]any{
		"system":        "extsim",
		"event":         "job.completed",
		"correlation":   map[string]string{"job.id": "job-42"},
		"expected_wait": expectedWait,
		"reason":        reason,
	})
	if res.IsError {
		t.Fatalf("subscribe_event returned an error: %s", resultText(t, res))
	}

	req := stub.lastRequest(t)
	if strings.Contains(string(req.Body), expectedWait) || strings.Contains(string(req.Body), reason) {
		t.Fatalf("forwarded body contains expected_wait or reason: %s", req.Body)
	}
	if _, ok := req.Query["expected_wait"]; ok {
		t.Fatalf("query contains expected_wait: %v", req.Query)
	}

	attrs, ok := recorder.findAttrs("platform MCP tool call")
	if !ok {
		t.Fatalf("no wait-intent log record; records: %v", recorder.messages())
	}
	if got := attrs["run_id"]; got != testRunID {
		t.Errorf("log run_id = %q, want %q", got, testRunID)
	}
	if got := attrs["tool"]; got != "subscribe_event" {
		t.Errorf("log tool = %q, want subscribe_event", got)
	}
	if got, _ := attrs["call_id"].(string); got == "" {
		t.Errorf("log call_id is empty; attrs: %v", attrs)
	}
	if got := attrs["expected_wait"]; got != expectedWait {
		t.Errorf("log expected_wait = %q, want %q", got, expectedWait)
	}
	if got := attrs["reason"]; got != reason {
		t.Errorf("log reason = %q, want %q", got, reason)
	}
}

func TestToolErrorResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tool      string
		args      map[string]any
		status    int
		body      string
		wantText  string
		wantRetry bool
	}{
		{
			name:     "list_events not found",
			tool:     "list_events",
			args:     map[string]any{"system": "extsim"},
			status:   http.StatusNotFound,
			body:     `{"error":"run not found"}`,
			wantText: "run not found",
		},
		{
			name:     "subscribe_event conflict",
			tool:     "subscribe_event",
			args:     map[string]any{"system": "extsim", "event": "job.completed", "correlation": map[string]string{"job.id": "job-42"}},
			status:   http.StatusConflict,
			body:     `{"error":"run already finished"}`,
			wantText: "run already finished",
		},
		{
			name:      "list_events server error",
			tool:      "list_events",
			status:    http.StatusInternalServerError,
			body:      `{"error":"database unavailable"}`,
			wantRetry: true,
		},
		{
			name:      "subscribe_event unavailable",
			tool:      "subscribe_event",
			args:      map[string]any{"system": "extsim", "event": "job.completed", "correlation": map[string]string{"job.id": "job-42"}},
			status:    http.StatusServiceUnavailable,
			body:      `{"error":"database unavailable"}`,
			wantRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newConnectorStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			srv := startTestServer(t, stub.URL(), nil, 0)
			session := connectTestClient(t, srv)

			res := callTool(t, session, tt.tool, tt.args)
			if !res.IsError {
				t.Fatalf("%s status %d: result was not marked as an error: %s", tt.tool, tt.status, resultText(t, res))
			}
			text := resultText(t, res)
			if tt.wantRetry {
				if !strings.Contains(strings.ToLower(text), "retry") {
					t.Errorf("error result does not say the call can be retried: %q", text)
				}
			} else if text != tt.wantText {
				t.Errorf("error result = %q, want the endpoint error %q", text, tt.wantText)
			}
		})
	}
}

func TestToolTimeoutIsReportedAsRetryable(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	stub := newConnectorStub(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	srv := startTestServer(t, stub.URL(), nil, 50*time.Millisecond)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "list_events", nil)
	if !res.IsError {
		t.Fatalf("timed out call was not marked as an error: %s", resultText(t, res))
	}
	if text := strings.ToLower(resultText(t, res)); !strings.Contains(text, "retry") {
		t.Errorf("timeout result does not say the call can be retried: %q", text)
	}
}

func TestConnectionFailureIsReportedAsRetryable(t *testing.T) {
	t.Parallel()

	// 端口 0 上没有监听器，连接稳定地被拒绝；不用刚关闭的临时端口，
	// 以免并行测试的监听器复用同一端口。
	srv := startTestServer(t, "127.0.0.1:0", nil, 0)
	session := connectTestClient(t, srv)

	res := callTool(t, session, "subscribe_event", map[string]any{
		"system":      "extsim",
		"event":       "job.completed",
		"correlation": map[string]string{"job.id": "job-42"},
	})
	if !res.IsError {
		t.Fatalf("unreachable connector call was not marked as an error: %s", resultText(t, res))
	}
	if text := strings.ToLower(resultText(t, res)); !strings.Contains(text, "retry") {
		t.Errorf("connection failure result does not say the call can be retried: %q", text)
	}
}

func TestStartValidation(t *testing.T) {
	t.Parallel()

	if _, err := Start(Config{RunID: "", ConnectorAddr: "127.0.0.1:1"}); err == nil {
		t.Error("Start without a run ID succeeded")
	}
	if _, err := Start(Config{RunID: testRunID, ConnectorAddr: ""}); err == nil {
		t.Error("Start without a connector address succeeded")
	}
	if _, err := Start(Config{RunID: testRunID, ConnectorAddr: "http://[::1"}); err == nil {
		t.Error("Start with an invalid connector address succeeded")
	}
}

func TestStartListenFailure(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if _, err := Start(Config{RunID: testRunID, ConnectorAddr: "127.0.0.1:1", Addr: listener.Addr().String()}); err == nil {
		t.Fatal("Start on an occupied address succeeded")
	}
}

func TestURLAndClose(t *testing.T) {
	t.Parallel()

	srv := startTestServer(t, "127.0.0.1:1", nil, 0)
	u := srv.URL()
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.HasSuffix(u, "/mcp") {
		t.Fatalf("URL = %q, want http://127.0.0.1:<port>/mcp", u)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// startTestServer 启动一个指向 stub 的服务；timeout 为 0 时用默认值。
func startTestServer(t *testing.T, connector string, logger *slog.Logger, timeout time.Duration) *Server {
	t.Helper()
	return startTestServerWithRunID(t, testRunID, connector, logger, timeout)
}

func startTestServerWithRunID(t *testing.T, runID, connector string, logger *slog.Logger, timeout time.Duration) *Server {
	t.Helper()
	cfg := Config{RunID: runID, ConnectorAddr: connector, Addr: "127.0.0.1:0", Logger: logger}
	var (
		srv *Server
		err error
	)
	if timeout == 0 {
		srv, err = Start(cfg)
	} else {
		srv, err = start(cfg, timeout)
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func connectTestClient(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL()}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %d items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

type recordedRequest struct {
	Method      string
	Path        string
	EscapedPath string
	Query       url.Values
	Header      http.Header
	Body        []byte
}

type connectorStub struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(http.ResponseWriter, *http.Request)
	url      string
}

func newConnectorStub(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *connectorStub {
	t.Helper()
	stub := &connectorStub{handler: handler}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

func (s *connectorStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		EscapedPath: r.URL.EscapedPath(),
		Query:       r.URL.Query(),
		Header:      r.Header.Clone(),
		Body:        body,
	})
	s.mu.Unlock()
	s.handler(w, r)
}

func (s *connectorStub) URL() string {
	return s.url
}

func (s *connectorStub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *connectorStub) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the connector stub received no request")
	}
	return s.requests[len(s.requests)-1]
}

type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.records))
	for _, record := range r.records {
		out = append(out, record.Message)
	}
	return out
}

func (r *logRecorder) findAttrs(message string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.Message != message {
			continue
		}
		attrs := make(map[string]any)
		record.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		return attrs, true
	}
	return nil, false
}
