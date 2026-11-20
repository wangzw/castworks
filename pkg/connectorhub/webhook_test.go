package connectorhub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testWebhookPayload = `{"job":{"id":"j-1","project":"demo","status":"completed",` +
	`"conclusion":"succeeded","updated_at":"2026-11-20T00:59:00Z"}}`

// newTestWebhookHandler 用照抄 extsim.md 的定义与一个签名密钥文件构造接收端。
func newTestWebhookHandler(t *testing.T, pool *pgxpool.Pool, signingKey string) *webhookHandler {
	t.Helper()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
	writeTestFile(t, filepath.Join(dir, "signing.key"), signingKey+"\n")
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
	handler := newWebhookHandler(config, pool)
	handler.now = testNow
	return handler
}

// serveWebhook 发出一次 POST。key 为空时不带签名头；deliveryID 或 eventType
// 为空时不带对应的头。
// webhookTestSubscription 是配置中项目 default 下的活动订阅。
func webhookTestSubscription(subID string) subscription {
	sub := defaultTestSubscription(subID)
	sub.ProjectID = "default"
	return sub
}

func serveWebhook(handler http.Handler, path, body, key, deliveryID, eventType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if deliveryID != "" {
		request.Header.Set("X-Extsim-Delivery", deliveryID)
	}
	if eventType != "" {
		request.Header.Set("X-Extsim-Event", eventType)
	}
	if key != "" {
		mac := hmac.New(sha256.New, []byte(key))
		_, _ = mac.Write([]byte(body))
		request.Header.Set("X-Extsim-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func countWebhookEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_event`).Scan(&count); err != nil {
		t.Fatalf("count connector_event: %v", err)
	}
	return count
}

func readWebhookEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deliveryID string) connectorEvent {
	t.Helper()
	ev, err := scanWebhookEvent(pool.QueryRow(ctx,
		`SELECT append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload
		   FROM connector_event WHERE delivery_id = $1`, deliveryID))
	if err != nil {
		t.Fatalf("read connector_event %s: %v", deliveryID, err)
	}
	return ev
}

func TestWebhookOversizedBodyReturnsRequestEntityTooLarge(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")

	// 请求体超过 1 MiB，签名本身正确；接收端必须在读取阶段就拒绝它。
	body := strings.Repeat("a", (1<<20)+1)
	recorder := serveWebhook(handler, "/hooks/extsim", body, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookEmptySigningKeyReturnsServiceUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
	}{
		{name: "empty file", content: ""},
		{name: "only a newline", content: "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := migratedPool(t)
			handler := newTestWebhookHandler(t, pool, "signing-key")
			path := handler.config.Systems[0].SigningKeyFile
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write signing key: %v", err)
			}

			recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", recorder.Code)
			}
			if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
				t.Fatalf("connector_event rows = %d, want 0", got)
			}
		})
	}
}

func TestWebhookWrongMethodReturnsMethodNotAllowed(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")

	request := httptest.NewRequest(http.MethodGet, "/hooks/extsim", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want %q", allow, http.MethodPost)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookWriteFailureReturns500AndWritesNothing(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	// 用一个只在本用例数据库中的触发器让写入失败。
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION connectorhub_test_fail_event_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected connector_event write failure';
		END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER connectorhub_test_fail_event_insert
		BEFORE INSERT ON connector_event
		FOR EACH ROW EXECUTE FUNCTION connectorhub_test_fail_event_insert()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	handler := newTestWebhookHandler(t, pool, "signing-key")
	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if got := countWebhookEvents(t, ctx, pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookUnknownPathReturnsNotFound(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")

	recorder := serveWebhook(handler, "/hooks/other", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookSigningKeyUnreadableReturnsServiceUnavailable(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")
	if err := os.Remove(handler.config.Systems[0].SigningKeyFile); err != nil {
		t.Fatalf("remove signing key: %v", err)
	}

	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookSignatureFailureReturnsUnauthorized(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")
	insertTestSubscription(t, t.Context(), pool, webhookTestSubscription("sub-1"))

	tests := []struct {
		name string
		key  string
	}{
		{name: "wrong key", key: "other-key"},
		{name: "missing header", key: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, tt.key, "d-1", "job.completed")
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
		})
	}

	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
	status, _, _ := readTestSubscription(t, t.Context(), pool, "sub-1")
	if status != "active" {
		t.Fatalf("subscription status = %q, want active", status)
	}
}

func TestWebhookMissingDeliveryIDReturnsBadRequest(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")

	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "", "job.completed")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookInvalidJSONReturnsBadRequest(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	handler := newTestWebhookHandler(t, pool, "signing-key")

	recorder := serveWebhook(handler, "/hooks/extsim", "not json", "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := countWebhookEvents(t, t.Context(), pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
}

func TestWebhookRecordsEventAndCompletesSubscription(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")

	sub := webhookTestSubscription("sub-1")
	insertTestSubscription(t, ctx, pool, sub)

	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	ev := readWebhookEvent(t, ctx, pool, "d-1")
	if ev.AppendSeq != 1 {
		t.Errorf("append_seq = %d, want 1", ev.AppendSeq)
	}
	if ev.System != "extsim" || ev.EventType != "job.completed" {
		t.Errorf("event identity = %q/%q", ev.System, ev.EventType)
	}
	if len(ev.ProjectIDs) != 1 || ev.ProjectIDs[0] != "default" {
		t.Errorf("project_ids = %v, want [default]", ev.ProjectIDs)
	}
	if ev.Correlation["job.id"] != "j-1" || ev.Correlation["job.project"] != "demo" {
		t.Errorf("correlation = %v", ev.Correlation)
	}
	wantOccurredAt := time.Date(2026, 11, 20, 0, 59, 0, 0, time.UTC)
	if !ev.OccurredAt.Equal(wantOccurredAt) {
		t.Errorf("occurred_at = %v, want %v", ev.OccurredAt, wantOccurredAt)
	}
	if string(ev.Payload) != testWebhookPayload {
		t.Errorf("payload = %q, want the request body", ev.Payload)
	}

	status, terminal, _ := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed/succeeded", status, terminal)
	}
	if got := countTestRows(t, ctx, pool, "subscription_claim", "sub-1"); got != 1 {
		t.Errorf("subscription_claim rows = %d, want 1", got)
	}
	completions := loadTestCompletions(t, ctx, pool, "sub-1")
	if len(completions) != 1 || completions[0].Source != "webhook" {
		t.Fatalf("completions = %+v, want one webhook completion", completions)
	}
}

func TestWebhookDuplicateDeliveryRematches(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")
	insertTestSubscription(t, ctx, pool, webhookTestSubscription("sub-1"))

	for i := 0; i < 2; i++ {
		recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
		if recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", i+1, recorder.Code)
		}
	}

	if got := countWebhookEvents(t, ctx, pool); got != 1 {
		t.Fatalf("connector_event rows = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 1 {
		t.Errorf("completion_delivery rows = %d, want 1", got)
	}
	if got := countTestRows(t, ctx, pool, "subscription_claim", "sub-1"); got != 1 {
		t.Errorf("subscription_claim rows = %d, want 1", got)
	}
}

func TestWebhookMatchFailureReturns500AndRetryCompletes(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")
	insertTestSubscription(t, ctx, pool, webhookTestSubscription("sub-1"))

	var once sync.Once
	handler.matchEvent = func(ctx context.Context, tx pgx.Tx, ev connectorEvent, terminal string, now time.Time) (int, error) {
		var injected error
		once.Do(func() { injected = errors.New("injected database failure") })
		if injected != nil {
			return 0, injected
		}
		return matchEvent(ctx, tx, ev, terminal, now)
	}

	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", recorder.Code)
	}
	if got := countWebhookEvents(t, ctx, pool); got != 1 {
		t.Fatalf("connector_event rows after failed match = %d, want 1", got)
	}
	status, _, _ := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "active" {
		t.Fatalf("subscription status after failed match = %q, want active", status)
	}

	recorder = serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", recorder.Code)
	}
	if got := countWebhookEvents(t, ctx, pool); got != 1 {
		t.Fatalf("connector_event rows after retry = %d, want 1", got)
	}
	status, terminal, _ := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "completed" || terminal != "succeeded" {
		t.Fatalf("subscription = status %q terminal %q, want completed/succeeded", status, terminal)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 1 {
		t.Errorf("completion_delivery rows = %d, want 1", got)
	}
}

func TestWebhookUnboundProjectIsRecordedWithoutMatching(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")
	insertTestSubscription(t, ctx, pool, webhookTestSubscription("sub-1"))

	payload := `{"job":{"id":"j-1","project":"other","status":"completed",` +
		`"conclusion":"succeeded","updated_at":"2026-11-20T00:59:00Z"}}`
	recorder := serveWebhook(handler, "/hooks/extsim", payload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	ev := readWebhookEvent(t, ctx, pool, "d-1")
	if len(ev.ProjectIDs) != 0 {
		t.Errorf("project_ids = %v, want empty", ev.ProjectIDs)
	}
	status, _, _ := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "active" {
		t.Fatalf("subscription status = %q, want active", status)
	}
	if got := countTestRows(t, ctx, pool, "completion_delivery", "sub-1"); got != 0 {
		t.Errorf("completion_delivery rows = %d, want 0", got)
	}
}

// fakeWebhookConnector 是只用于 webhook 摄入测试的连接器：事件类型由测试
// 指定，签名校验恒为通过。
type fakeWebhookConnector struct {
	eventType string
}

func (fakeWebhookConnector) VerifySignature(http.Header, []byte, []byte) bool { return true }

func (f fakeWebhookConnector) EventType(http.Header) (string, error) { return f.eventType, nil }

func (fakeWebhookConnector) Terminal(body []byte) (string, error) {
	return jobConclusion(body)
}

func (fakeWebhookConnector) CheckStatus(context.Context, string, map[string]string) (string, bool, []byte, error) {
	return "", false, nil, nil
}

func TestWebhookUnknownEventTypeIsDropped(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	dir := t.TempDir()
	signingKeyFile := filepath.Join(dir, "signing.key")
	writeTestFile(t, signingKeyFile, "signing-key\n")

	definition := Definition{
		Kind: definitionKind,
		Name: "extsim",
		Events: Events{
			Sources: []EventSource{{
				Type: "webhook", Path: "/hooks/extsim", Signature: signatureHMACSHA256,
				DeliveryID: "X-Extsim-Delivery", EventTypes: []string{"job.completed"},
			}},
			CorrelationKeys: []string{"job.project", "job.id"},
			IdentityKeys:    []string{"job.id"},
			ProjectBinding:  "job.project",
			DedupKey:        []string{"event_type", "job.id"},
			Ordering:        Ordering{OccurredAt: "job.updated_at"},
		},
	}
	config := Config{
		Project: "default",
		Systems: []System{{
			Name:           "extsim",
			Definition:     definition,
			SigningKeyFile: signingKeyFile,
			Connector:      fakeWebhookConnector{eventType: "job.unknown"},
		}},
		Bindings: map[string][]string{"extsim": {"demo"}},
	}
	handler := newWebhookHandler(config, pool)
	handler.now = testNow

	var log bytes.Buffer
	handler.logger = slog.New(slog.NewTextHandler(&log, nil))

	recorder := serveWebhook(handler, "/hooks/extsim", testWebhookPayload, "signing-key", "d-1", "job.unknown")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := countWebhookEvents(t, ctx, pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
	if lines := strings.Count(log.String(), "\n"); lines != 1 {
		t.Fatalf("log lines = %d, want 1: %q", lines, log.String())
	}
	if !strings.Contains(log.String(), "job.unknown") {
		t.Errorf("log %q does not mention the event type", log.String())
	}
}

func TestWebhookMissingConclusionReturnsBadRequest(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")
	insertTestSubscription(t, ctx, pool, webhookTestSubscription("sub-1"))

	payload := `{"job":{"id":"j-1","project":"demo","updated_at":"2026-11-20T00:59:00Z"}}`
	recorder := serveWebhook(handler, "/hooks/extsim", payload, "signing-key", "d-1", "job.completed")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := countWebhookEvents(t, ctx, pool); got != 0 {
		t.Fatalf("connector_event rows = %d, want 0", got)
	}
	status, _, _ := readTestSubscription(t, ctx, pool, "sub-1")
	if status != "active" {
		t.Fatalf("subscription status = %q, want active", status)
	}
}

func TestWebhookConcurrentWritesAllocateAppendSeqInOrder(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	handler := newTestWebhookHandler(t, pool, "signing-key")

	const requests = 8
	codes := make([]int, requests)
	var wait sync.WaitGroup
	for i := 0; i < requests; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			payload := `{"job":{"id":"j-` + string(rune('a'+i)) + `","project":"demo",` +
				`"status":"completed","conclusion":"succeeded","updated_at":"2026-11-20T00:59:00Z"}}`
			recorder := serveWebhook(handler, "/hooks/extsim", payload, "signing-key",
				"d-"+string(rune('a'+i)), "job.completed")
			codes[i] = recorder.Code
		}(i)
	}
	wait.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, code)
		}
	}

	rows, err := pool.Query(ctx, `SELECT append_seq, delivery_id FROM connector_event ORDER BY append_seq`)
	if err != nil {
		t.Fatalf("query connector_event: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	deliveries := make(map[string]bool)
	for rows.Next() {
		var seq int64
		var delivery string
		if err := rows.Scan(&seq, &delivery); err != nil {
			t.Fatalf("scan connector_event: %v", err)
		}
		seqs = append(seqs, seq)
		deliveries[delivery] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read connector_event: %v", err)
	}
	if len(seqs) != requests {
		t.Fatalf("connector_event rows = %d, want %d", len(seqs), requests)
	}
	for i, seq := range seqs {
		if want := int64(i + 1); seq != want {
			t.Fatalf("append_seq[%d] = %d, want %d; all = %v", i, seq, want, seqs)
		}
	}
	if len(deliveries) != requests {
		t.Fatalf("distinct delivery ids = %d, want %d", len(deliveries), requests)
	}
}

// TestWebhookTrimSigningKey 验证签名密钥文件末尾换行、单个 \r 的处理。
func TestWebhookTrimSigningKey(t *testing.T) {
	t.Parallel()

	t.Run("line feed", func(t *testing.T) {
		t.Parallel()

		if got := string(trimSigningKey([]byte("signing-key\n"))); got != "signing-key" {
			t.Fatalf("trimSigningKey() = %q, want %q", got, "signing-key")
		}
	})

	t.Run("carriage return and line feed", func(t *testing.T) {
		t.Parallel()

		if got := string(trimSigningKey([]byte("signing-key\r\n"))); got != "signing-key" {
			t.Fatalf("trimSigningKey() = %q, want %q", got, "signing-key")
		}
	})

	t.Run("only one trailing newline is removed", func(t *testing.T) {
		t.Parallel()

		if got := string(trimSigningKey([]byte("abc\n\r\n"))); got != "abc\n" {
			t.Fatalf("trimSigningKey() = %q, want %q", got, "abc\n")
		}
	})

	t.Run("trailing carriage return is kept", func(t *testing.T) {
		t.Parallel()

		raw := []byte("signing-key\r")
		key := trimSigningKey(raw)
		if !bytes.Equal(key, raw) {
			t.Fatalf("trimSigningKey() = %q, want the raw bytes %q", key, raw)
		}

		// 签名使用保留的 \r 计算，结果与按原字节计算的一致。
		signed := hmac.New(sha256.New, key)
		_, _ = signed.Write([]byte(testWebhookPayload))
		rawSigned := hmac.New(sha256.New, raw)
		_, _ = rawSigned.Write([]byte(testWebhookPayload))
		if !hmac.Equal(signed.Sum(nil), rawSigned.Sum(nil)) {
			t.Fatalf("signature with the retained \\r differs from the signature over the raw bytes")
		}
	})
}
