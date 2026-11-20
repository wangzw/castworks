package connectorhub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

const hubTestSigningKey = "signing-key"

// hubInboxCall 记录一次 AppendInbox 调用。
type hubInboxCall struct {
	runID string
	event runcontroller.InboxEvent
}

// hubFakeRuns 是 Runs 的测试替身。details 之外的运行实例返回 ErrNotFound。
type hubFakeRuns struct {
	mu        sync.Mutex
	details   map[string]runcontroller.RunDetail
	getErr    error
	appendErr error
	calls     []hubInboxCall
}

func (f *hubFakeRuns) Get(_ context.Context, runID string) (runcontroller.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return runcontroller.RunDetail{}, f.getErr
	}
	if detail, ok := f.details[runID]; ok {
		return detail, nil
	}
	return runcontroller.RunDetail{}, runcontroller.ErrNotFound
}

func (f *hubFakeRuns) AppendInbox(_ context.Context, runID string, ev runcontroller.InboxEvent) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hubInboxCall{runID: runID, event: ev})
	if f.appendErr != nil {
		return 0, false, f.appendErr
	}
	return int64(len(f.calls)), false, nil
}

func (f *hubFakeRuns) callsSnapshot() []hubInboxCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hubInboxCall(nil), f.calls...)
}

// newTestHub 用与 webhook 测试相同的定义与签名密钥构造 Hub，项目为 default。
func newTestHub(t *testing.T, pool *pgxpool.Pool, runs Runs, opts ...Option) *Hub {
	t.Helper()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
	writeTestFile(t, filepath.Join(dir, "signing.key"), hubTestSigningKey+"\n")
	configPath := filepath.Join(dir, "connectorhub.yaml")
	writeTestFile(t, configPath, `project: default
systems:
  - definition: extsim.yaml
    signing_key_file: signing.key
    api_base_url: http://127.0.0.1:9100
bindings:
  extsim: [demo]
`)

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	hub, err := New(config, pool, runs, append([]Option{WithClock(testNow)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return hub
}

func activeRunDetail(runID string) runcontroller.RunDetail {
	return runcontroller.RunDetail{
		RunSummary: runcontroller.RunSummary{RunID: runID, State: runcontroller.StateActive},
	}
}

// TestHubWebhookDeliversCompletionToInbox 以 Runs 替身走通 webhook 到
// AppendInbox 的完整路径，并核对 InboxEvent 的每个字段与 connectorhub.md
// “订阅完成事件”一节一致。
func TestHubWebhookDeliversCompletionToInbox(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	sub := defaultTestSubscription("sub-1")
	sub.ProjectID = "default"
	insertTestSubscription(t, ctx, pool, sub)

	runs := &hubFakeRuns{details: map[string]runcontroller.RunDetail{
		"run-1": activeRunDetail("run-1"),
	}}
	hub := newTestHub(t, pool, runs)

	recorder := serveWebhook(hub.WebhookHandler(), "/hooks/extsim",
		testWebhookPayload, hubTestSigningKey, "d-1", "job.completed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", recorder.Code, recorder.Body.String())
	}

	calls := runs.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("AppendInbox calls = %d, want 1 after the webhook request", len(calls))
	}
	call := calls[0]
	if call.runID != "run-1" {
		t.Fatalf("AppendInbox run id = %q, want run-1", call.runID)
	}

	ev := call.event
	if ev.Kind != runcontroller.KindSubscriptionCompleted {
		t.Errorf("Kind = %q, want %q", ev.Kind, runcontroller.KindSubscriptionCompleted)
	}
	if ev.Text != "" {
		t.Errorf("Text = %q, want empty for subscription_completed", ev.Text)
	}
	wantEventID := encodeTuple("sub-1", "succeeded")
	if ev.EventID != wantEventID {
		t.Errorf("EventID = %q, want %q", ev.EventID, wantEventID)
	}
	wantDedupKey := hashTuple("sub-1", hashTuple("job.completed", "j-1", "succeeded"))
	if ev.DedupKey != wantDedupKey {
		t.Errorf("DedupKey = %q, want %q", ev.DedupKey, wantDedupKey)
	}
	if ev.Subscription == nil {
		t.Fatal("Subscription is nil")
	}

	sc := ev.Subscription
	if sc.SubID != "sub-1" || sc.System != "extsim" || sc.Event != "job.completed" {
		t.Errorf("Subscription identity = (%q, %q, %q), want (sub-1, extsim, job.completed)",
			sc.SubID, sc.System, sc.Event)
	}
	if sc.SubscriptionStatus != "completed" {
		t.Errorf("SubscriptionStatus = %q, want completed", sc.SubscriptionStatus)
	}
	if sc.Terminal != "succeeded" {
		t.Errorf("Terminal = %q, want succeeded", sc.Terminal)
	}
	wantCorrelation := map[string]string{"job.id": "j-1"}
	if !reflect.DeepEqual(sc.Correlation, wantCorrelation) {
		t.Errorf("Correlation = %v, want %v", sc.Correlation, wantCorrelation)
	}
	wantOccurredAt := time.Date(2026, 11, 20, 0, 59, 0, 0, time.UTC)
	if !sc.OccurredAt.Equal(wantOccurredAt) {
		t.Errorf("OccurredAt = %v, want %v", sc.OccurredAt, wantOccurredAt)
	}
	if sc.Source != "webhook" {
		t.Errorf("Source = %q, want webhook", sc.Source)
	}
	if sc.Detail != testWebhookPayload {
		t.Errorf("Detail = %q, want the original payload", sc.Detail)
	}
	if sc.DetailTruncated {
		t.Error("DetailTruncated = true, want false")
	}

	state := readTestDeliveryState(t, ctx, pool, sub.SubID, ev.DedupKey)
	if state.Status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", state.Status)
	}
	if state.Attempts != 1 {
		t.Errorf("delivery attempts = %d, want 1", state.Attempts)
	}
}

// TestCompletionConstructorsSatisfyInboxConditions 断言两个事件构造函数产出的
// 事件满足 AppendInbox 对 subscription_completed 的全部条件。
func TestCompletionConstructorsSatisfyInboxConditions(t *testing.T) {
	t.Parallel()

	sub := defaultTestSubscription("sub-1")
	sub.ProjectID = "default"
	occurredAt := testNow().Add(-time.Minute)
	ev := defaultTestEvent(1, "d-1", occurredAt)

	webhookCompletion := newWebhookCompletion(sub, ev, "succeeded")
	assertInboxEventConditions(t, "webhook", webhookCompletion)
	if webhookCompletion.Source != "webhook" {
		t.Errorf("webhook Source = %q, want webhook", webhookCompletion.Source)
	}
	if !webhookCompletion.OccurredAt.Equal(occurredAt) {
		t.Errorf("webhook OccurredAt = %v, want the event occurred_at %v",
			webhookCompletion.OccurredAt, occurredAt)
	}

	queriedAt := testNow()
	verifyCompletion := newVerifyCompletion(sub, "succeeded", []byte(testJobPayload), queriedAt)
	assertInboxEventConditions(t, "verify", verifyCompletion)
	if verifyCompletion.Source != "verify" {
		t.Errorf("verify Source = %q, want verify", verifyCompletion.Source)
	}
	if !verifyCompletion.OccurredAt.Equal(queriedAt) {
		t.Errorf("verify OccurredAt = %v, want the query time %v",
			verifyCompletion.OccurredAt, queriedAt)
	}
}

func assertInboxEventConditions(t *testing.T, name string, c Completion) {
	t.Helper()

	for field, value := range map[string]string{
		"SubID":              c.SubID,
		"System":             c.System,
		"Event":              c.Event,
		"SubscriptionStatus": c.SubscriptionStatus,
		"Terminal":           c.Terminal,
		"Source":             c.Source,
		"EventID":            c.EventID,
		"DedupKey":           c.DedupKey,
	} {
		if value == "" {
			t.Errorf("%s: %s is empty", name, field)
		}
	}
	if c.OccurredAt.IsZero() {
		t.Errorf("%s: OccurredAt is zero", name)
	} else if err := timestamppb.New(c.OccurredAt).CheckValid(); err != nil {
		t.Errorf("%s: OccurredAt %v is invalid: %v", name, c.OccurredAt, err)
	}
	correlationBytes := 0
	for key, value := range c.Correlation {
		if key == "" {
			t.Errorf("%s: correlation contains an empty key", name)
		}
		correlationBytes += len(key) + len(value)
	}
	if correlationBytes > runcontroller.MaxCorrelationBytes {
		t.Errorf("%s: correlation is %d bytes, want at most %d",
			name, correlationBytes, runcontroller.MaxCorrelationBytes)
	}
	if len(c.Detail) > runcontroller.MaxDetailBytes {
		t.Errorf("%s: detail is %d bytes, want at most %d",
			name, len(c.Detail), runcontroller.MaxDetailBytes)
	}
}

// TestHubDeliveryKeepsPendingOnInvalidEvent 覆盖 AppendInbox 返回
// ErrInvalidEvent：记录保持 pending、attempts 加一、写入 last_error，并记
// 一行错误日志。
func TestHubDeliveryKeepsPendingOnInvalidEvent(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	runs := &hubFakeRuns{
		details:   map[string]runcontroller.RunDetail{"run-1": activeRunDetail("run-1")},
		appendErr: runcontroller.ErrInvalidEvent,
	}
	hub := newTestHub(t, pool, runs, WithLogger(logger))

	completion := testCompletion("sub-invalid")
	insertTestCompletion(t, ctx, pool, "run-1", completion)

	if err := hub.delivery.deliverPending(ctx); err != nil {
		t.Fatalf("deliverPending: %v", err)
	}

	if calls := runs.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("AppendInbox calls = %d, want 1", len(calls))
	}
	state := readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
	if state.Status != "pending" {
		t.Errorf("status = %q, want pending", state.Status)
	}
	if state.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", state.Attempts)
	}
	if !strings.Contains(state.LastError, runcontroller.ErrInvalidEvent.Error()) {
		t.Errorf("last_error = %q, want it to mention the invalid event", state.LastError)
	}
	if !strings.Contains(logs.String(), runcontroller.ErrInvalidEvent.Error()) {
		t.Errorf("logs = %q, want one error line mentioning the invalid event", logs.String())
	}
}

// TestHubRunReturnsWhenContextCanceled 断言 Run 在 ctx 取消后返回。
func TestHubRunReturnsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	hub := newTestHub(t, pool, &hubFakeRuns{details: map[string]runcontroller.RunDetail{}})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- hub.Run(ctx)
	}()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Hub.Run = %v, want nil after context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hub.Run did not return after context cancellation")
	}
}

// TestHubWebhookDoesNotDeliverForRejectedRequests 断言 401、404 与 405 的
// 请求不会触发任何投递，遗留的 pending 记录保持原样。
func TestHubWebhookDoesNotDeliverForRejectedRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		path   string
		key    string
		want   int
	}{
		{name: "unauthorized", method: http.MethodPost, path: "/hooks/extsim", key: "wrong-key", want: http.StatusUnauthorized},
		{name: "unknown path", method: http.MethodPost, path: "/hooks/unknown", key: hubTestSigningKey, want: http.StatusNotFound},
		{name: "wrong method", method: http.MethodGet, path: "/hooks/extsim", want: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := migratedPool(t)
			ctx := t.Context()
			runs := &hubFakeRuns{details: map[string]runcontroller.RunDetail{
				"run-1": activeRunDetail("run-1"),
			}}
			hub := newTestHub(t, pool, runs)

			completion := testCompletion("sub-pending")
			insertTestCompletion(t, ctx, pool, "run-1", completion)

			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(testWebhookPayload))
			if tc.method == http.MethodPost {
				mac := hmac.New(sha256.New, []byte(tc.key))
				_, _ = mac.Write([]byte(testWebhookPayload))
				request.Header.Set("X-Extsim-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				request.Header.Set("X-Extsim-Delivery", "d-1")
				request.Header.Set("X-Extsim-Event", "job.completed")
			}
			recorder := httptest.NewRecorder()
			hub.WebhookHandler().ServeHTTP(recorder, request)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
			if calls := runs.callsSnapshot(); len(calls) != 0 {
				t.Fatalf("AppendInbox calls = %d, want 0 for a rejected request", len(calls))
			}
			state := readTestDeliveryState(t, ctx, pool, completion.SubID, completion.DedupKey)
			if state.Status != "pending" || state.Attempts != 0 {
				t.Fatalf("delivery = %q attempts %d, want pending/0", state.Status, state.Attempts)
			}
		})
	}
}

// TestHubWebhookDeliversOnlyRecordsWrittenByTheRequest 断言立即投递只处理
// 本次请求写入的记录，遗留的 pending 记录交给后台扫描。
func TestHubWebhookDeliversOnlyRecordsWrittenByTheRequest(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	runs := &hubFakeRuns{details: map[string]runcontroller.RunDetail{
		"run-1": activeRunDetail("run-1"),
	}}
	hub := newTestHub(t, pool, runs)

	stale1 := testCompletion("sub-stale-1")
	stale2 := testCompletion("sub-stale-2")
	insertTestCompletion(t, ctx, pool, "run-1", stale1)
	insertTestCompletion(t, ctx, pool, "run-1", stale2)

	sub := webhookTestSubscription("sub-request")
	insertTestSubscription(t, ctx, pool, sub)

	recorder := serveWebhook(hub.WebhookHandler(), "/hooks/extsim",
		testWebhookPayload, hubTestSigningKey, "d-1", "job.completed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", recorder.Code, recorder.Body.String())
	}

	calls := runs.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("AppendInbox calls = %d, want 1 for the record written by the request", len(calls))
	}
	if calls[0].event.Subscription == nil || calls[0].event.Subscription.SubID != sub.SubID {
		t.Fatalf("delivered subscription = %+v, want %s", calls[0].event.Subscription, sub.SubID)
	}
	for _, stale := range []Completion{stale1, stale2} {
		state := readTestDeliveryState(t, ctx, pool, stale.SubID, stale.DedupKey)
		if state.Status != "pending" || state.Attempts != 0 {
			t.Fatalf("%s delivery = %q attempts %d, want pending/0", stale.SubID, state.Status, state.Attempts)
		}
	}
}

// TestNewRequiresDependencies 覆盖 New 对存储与运行实例服务的非空校验。
func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()

	runs := &hubFakeRuns{}
	if _, err := New(Config{}, nil, runs); err == nil {
		t.Fatal("New without a pool returned no error")
	}

	pool := migratedPool(t)
	if _, err := New(Config{}, pool, nil); err == nil {
		t.Fatal("New without a run service returned no error")
	}
	if _, err := New(Config{Project: "demo"}, pool, runs); err != nil {
		t.Fatalf("New: %v", err)
	}
}
