package connectorhub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// fakeConnector 是测试用的连接器，签名与事件类型总是通过，状态查询结果
// 由字段注入。
type fakeConnector struct {
	terminal        string
	reached         bool
	body            []byte
	err             error
	calls           int
	lastAPI         string
	lastCorrelation map[string]string
}

func (*fakeConnector) VerifySignature(http.Header, []byte, []byte) bool { return true }

func (*fakeConnector) EventType(http.Header) (string, error) { return "job.completed", nil }

func (*fakeConnector) Terminal(body []byte) (string, error) { return jobConclusion(body) }

func (f *fakeConnector) CheckStatus(_ context.Context, api string, correlation map[string]string) (string, bool, []byte, error) {
	f.calls++
	f.lastAPI = api
	f.lastCorrelation = correlation
	if f.err != nil {
		return "", false, nil, f.err
	}
	if !f.reached {
		return "", false, f.body, nil
	}
	return f.terminal, true, f.body, nil
}

// fakeRuns 是测试用的运行实例查询。details 之外的运行实例返回 ErrNotFound。
type fakeRuns struct {
	details map[string]runcontroller.RunDetail
	err     error
}

func (f *fakeRuns) Get(_ context.Context, runID string) (runcontroller.RunDetail, error) {
	if f.err != nil {
		return runcontroller.RunDetail{}, f.err
	}
	detail, ok := f.details[runID]
	if !ok {
		return runcontroller.RunDetail{}, runcontroller.ErrNotFound
	}
	return detail, nil
}

func runsWithStates(states map[string]runcontroller.State) *fakeRuns {
	details := make(map[string]runcontroller.RunDetail, len(states))
	for runID, state := range states {
		details[runID] = runcontroller.RunDetail{
			RunSummary: runcontroller.RunSummary{RunID: runID, State: state},
		}
	}
	return &fakeRuns{details: details}
}

// newTestEndpoint 构造以 testdata/extsim.yaml 为定义、demo 为项目的端点处理器。
func newTestEndpoint(t *testing.T, connector Connector, runs runGetter) (*connectorHandler, *pgxpool.Pool) {
	t.Helper()

	definition, err := loadDefinition("testdata/extsim.yaml")
	if err != nil {
		t.Fatalf("load definition: %v", err)
	}
	config := &Config{
		Project: "demo",
		Systems: []System{{
			Name:          definition.Name,
			Definition:    definition,
			Connector:     connector,
			BindingValues: []string{"demo"},
		}},
		Bindings: map[string][]string{"extsim": {"demo"}},
	}
	pool := migratedPool(t)
	handler := newConnectorHandler(config, pool, runs)
	handler.now = testNow
	return handler, pool
}

func doEndpointRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	return doEndpointRawRequest(t, handler, method, path, reader)
}

func doEndpointRawRequest(t *testing.T, handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if body.Error == "" {
		t.Fatalf("error body is empty: %q", rec.Body.String())
	}
	return body.Error
}

// appendTestConnectorEvent 以 webhook 摄入的方式分配 append_seq 并写入一条
// 连接器事件：事务内锁住计数行、取当前 next_seq、插入事件并递增计数器。
func appendTestConnectorEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ev connectorEvent) connectorEvent {
	t.Helper()

	correlation := ev.Correlation
	if correlation == nil {
		correlation = map[string]string{}
	}
	encoded, err := json.Marshal(correlation)
	if err != nil {
		t.Fatalf("encode event correlation: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event transaction: %v", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx,
		`UPDATE connector_event_counter SET next_seq = next_seq + 1 RETURNING next_seq - 1`,
	).Scan(&seq); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("allocate append_seq: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO connector_event
		(append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`,
		seq, ev.System, ev.DeliveryID, ev.EventType, ev.ProjectIDs,
		string(encoded), ev.OccurredAt, ev.Payload); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert event %s: %v", ev.DeliveryID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event %s: %v", ev.DeliveryID, err)
	}
	ev.AppendSeq = seq
	return ev
}

func TestEndpointCatalog(t *testing.T) {
	t.Parallel()

	handler, _ := newTestEndpoint(t, &fakeConnector{},
		runsWithStates(map[string]runcontroller.State{"run-1": runcontroller.StateActive}))

	for _, path := range []string{
		"/v1/runs/run-1/events",
		"/v1/runs/run-1/events?system=extsim",
	} {
		rec := doEndpointRequest(t, handler, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
		if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("%s Content-Type = %q, want application/json", path, contentType)
		}
		if !strings.Contains(rec.Body.String(), `"correlation_keys"`) {
			t.Fatalf("%s body has no snake_case correlation_keys: %s", path, rec.Body.String())
		}

		var descriptors []EventDescriptor
		if err := json.Unmarshal(rec.Body.Bytes(), &descriptors); err != nil {
			t.Fatalf("%s decode descriptor list: %v", path, err)
		}
		if len(descriptors) != 1 {
			t.Fatalf("%s descriptors = %v, want one", path, descriptors)
		}
		got := descriptors[0]
		want := EventDescriptor{
			System:          "extsim",
			Event:           "job.completed",
			CorrelationKeys: []string{"job.project", "job.id"},
			IdentityKeys:    []string{"job.id"},
			Terminal:        []string{"succeeded", "failed"},
			Lifetime:        []string{"once"},
			Verify:          true,
		}
		if got.Description == "" || !strings.Contains(got.Description, "job.completed") {
			t.Errorf("%s description = %q, want an agent-facing description", path, got.Description)
		}
		got.Description = ""
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s descriptor = %+v, want %+v", path, got, want)
		}
	}
}

func TestEndpointCatalogRejectsUnknownSystem(t *testing.T) {
	t.Parallel()

	handler, _ := newTestEndpoint(t, &fakeConnector{},
		runsWithStates(map[string]runcontroller.State{"run-1": runcontroller.StateActive}))

	rec := doEndpointRequest(t, handler, http.MethodGet, "/v1/runs/run-1/events?system=github", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if message := decodeErrorBody(t, rec); !strings.Contains(message, "not available") {
		t.Fatalf("error = %q, want it to say the system is not available", message)
	}
}

func TestEndpointRunStateErrors(t *testing.T) {
	t.Parallel()

	handler, _ := newTestEndpoint(t, &fakeConnector{}, runsWithStates(map[string]runcontroller.State{
		"run-active": runcontroller.StateActive,
		"run-ended":  runcontroller.StateEnded,
		"run-failed": runcontroller.StateFailed,
	}))

	cases := []struct {
		name   string
		path   string
		status int
		phrase string
	}{
		{name: "missing events", path: "/v1/runs/run-missing/events", status: http.StatusNotFound, phrase: "run not found"},
		{name: "missing subscribe", path: "/v1/runs/run-missing/subscriptions", status: http.StatusNotFound, phrase: "run not found"},
		{name: "ended events", path: "/v1/runs/run-ended/events", status: http.StatusConflict, phrase: "ended or failed"},
		{name: "failed events", path: "/v1/runs/run-failed/events", status: http.StatusConflict, phrase: "ended or failed"},
		{name: "ended subscribe", path: "/v1/runs/run-ended/subscriptions", status: http.StatusConflict, phrase: "ended or failed"},
	}
	for _, tc := range cases {
		method := http.MethodGet
		var body any
		if strings.HasSuffix(tc.path, "/subscriptions") {
			method = http.MethodPost
			body = subscribeRequest{System: "extsim", Event: "job.completed", Correlation: map[string]string{"job.id": "j-1"}}
		}
		rec := doEndpointRequest(t, handler, method, tc.path, body)
		if rec.Code != tc.status {
			t.Errorf("%s status = %d, want %d: %s", tc.name, rec.Code, tc.status, rec.Body.String())
			continue
		}
		if message := decodeErrorBody(t, rec); !strings.Contains(message, tc.phrase) {
			t.Errorf("%s error = %q, want it to contain %q", tc.name, message, tc.phrase)
		}
	}
}

func TestEndpointRouting(t *testing.T) {
	t.Parallel()

	handler, _ := newTestEndpoint(t, &fakeConnector{},
		runsWithStates(map[string]runcontroller.State{"run-1": runcontroller.StateActive}))

	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "unknown path", method: http.MethodGet, path: "/v1/runs/run-1/other", status: http.StatusNotFound},
		{name: "missing run id", method: http.MethodGet, path: "/v1/runs//events", status: http.StatusNotFound},
		{name: "wrong method events", method: http.MethodPost, path: "/v1/runs/run-1/events", status: http.StatusMethodNotAllowed},
		{name: "wrong method subscribe", method: http.MethodGet, path: "/v1/runs/run-1/subscriptions", status: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		rec := doEndpointRawRequest(t, handler, tc.method, tc.path, nil)
		if rec.Code != tc.status {
			t.Errorf("%s status = %d, want %d: %s", tc.name, rec.Code, tc.status, rec.Body.String())
		}
	}
}
