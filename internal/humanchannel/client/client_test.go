package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient starts a test server with h and returns a client for it.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatalf("New(%q): %v", srv.URL, err)
	}
	return c
}

// record carries what the server saw for one request.
type record struct {
	method      string
	path        string
	contentType string
	body        []byte
}

func recordRequest(r *http.Request) record {
	body, _ := io.ReadAll(r.Body)
	return record{
		method:      r.Method,
		path:        r.URL.EscapedPath(),
		contentType: r.Header.Get("Content-Type"),
		body:        body,
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestNew(t *testing.T) {
	t.Parallel()

	valid := []string{
		"http://127.0.0.1:8080",
		"https://example.com",
		"http://[::1]:8080/",
	}
	for _, baseURL := range valid {
		c, err := New(baseURL, nil)
		if err != nil {
			t.Errorf("New(%q): unexpected error: %v", baseURL, err)
			continue
		}
		if c == nil {
			t.Errorf("New(%q) = nil client", baseURL)
		}
	}

	invalid := []string{
		"",
		"127.0.0.1:8080",
		"ftp://example.com",
		"http://",
		"http:///v1",
		"not a url",
		"://bad",
	}
	for _, baseURL := range invalid {
		if _, err := New(baseURL, nil); err == nil {
			t.Errorf("New(%q) = nil error, want error", baseURL)
		}
	}
}

func TestNewClients(t *testing.T) {
	t.Parallel()

	c, err := New("http://127.0.0.1:8080", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.http.Timeout != 5*time.Minute {
		t.Errorf("default client timeout = %v, want %v", c.http.Timeout, 5*time.Minute)
	}

	customCheckRedirect := func(*http.Request, []*http.Request) error { return nil }
	custom := &http.Client{Timeout: 30 * time.Second, CheckRedirect: customCheckRedirect}
	c, err = New("http://127.0.0.1:8080", custom)
	if err != nil {
		t.Fatalf("New with custom client: %v", err)
	}
	if c.http == custom {
		t.Error("New used the caller's http.Client directly, want a copy")
	}
	if custom.CheckRedirect == nil {
		t.Error("New cleared the caller's CheckRedirect")
	}
	if c.http.Timeout != custom.Timeout {
		t.Errorf("copied client timeout = %v, want %v", c.http.Timeout, custom.Timeout)
	}
	if c.http.CheckRedirect == nil {
		t.Error("New did not set CheckRedirect on its own client")
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()

	records := make(chan record, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"run_id":"run-1"}`)
	})

	runID, err := c.Create(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if runID != "run-1" {
		t.Errorf("runID = %q, want %q", runID, "run-1")
	}

	got := <-records
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want %q", got.method, http.MethodPost)
	}
	if got.path != "/v1/runs" {
		t.Errorf("path = %q, want %q", got.path, "/v1/runs")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got.contentType, "application/json")
	}
	if string(got.body) != `{"prompt":"hello"}` {
		t.Errorf("body = %q, want %q", got.body, `{"prompt":"hello"}`)
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	const body = `[{"run_id":"run-1","state":"running","substate":"","created_at":"2026-11-19T06:30:00.123Z"},{"run_id":"run-2","state":"ended","substate":"finished","created_at":"2026-11-19T06:31:00.000Z"}]`
	records := make(chan record, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})

	runs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].RunID != "run-1" || runs[0].State != "running" || runs[0].Substate != "" {
		t.Errorf("runs[0] = %+v, want run-1 running with empty substate", runs[0])
	}
	if want := mustParseTime(t, "2026-11-19T06:30:00.123Z"); !runs[0].CreatedAt.Equal(want) {
		t.Errorf("runs[0].CreatedAt = %v, want %v", runs[0].CreatedAt.Time, want)
	}
	if runs[1].RunID != "run-2" || runs[1].State != "ended" || runs[1].Substate != "finished" {
		t.Errorf("runs[1] = %+v, want run-2 ended finished", runs[1])
	}
	if want := mustParseTime(t, "2026-11-19T06:31:00.000Z"); !runs[1].CreatedAt.Equal(want) {
		t.Errorf("runs[1].CreatedAt = %v, want %v", runs[1].CreatedAt.Time, want)
	}

	got := <-records
	if got.method != http.MethodGet {
		t.Errorf("method = %q, want %q", got.method, http.MethodGet)
	}
	if got.path != "/v1/runs" {
		t.Errorf("path = %q, want %q", got.path, "/v1/runs")
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	const body = `{
  "run_id": "run-1",
  "state": "suspended",
  "substate": "waiting_for_human",
  "created_at": "2026-11-19T06:30:00.123Z",
  "failure_reason": "",
  "turns": [
    {"turn_id": "turn-1", "started_at": "2026-11-19T06:30:01.000Z", "ended_at": "2026-11-19T06:30:02.500Z", "output": "hello\nworld", "is_error": false},
    {"turn_id": "turn-2", "started_at": "2026-11-19T06:30:03.000Z", "ended_at": null, "output": "", "is_error": true}
  ],
  "suspensions": [
    {"started_at": "2026-11-19T06:30:04.000Z", "snapshot_ms": null, "restore_ms": null, "hello_ms": null, "error": ""},
    {"started_at": "2026-11-19T06:31:00.000Z", "snapshot_ms": 123, "restore_ms": 456, "hello_ms": 789, "error": "restore failed"}
  ]
}`
	records := make(chan record, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})

	detail, err := c.Get(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.RunID != "run-1" || detail.State != "suspended" || detail.Substate != "waiting_for_human" {
		t.Errorf("detail = %+v, want run-1 suspended waiting_for_human", detail)
	}
	if want := mustParseTime(t, "2026-11-19T06:30:00.123Z"); !detail.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", detail.CreatedAt.Time, want)
	}
	if detail.FailureReason != "" {
		t.Errorf("FailureReason = %q, want empty", detail.FailureReason)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(detail.Turns))
	}
	turn := detail.Turns[0]
	if turn.TurnID != "turn-1" || turn.Output != "hello\nworld" || turn.IsError {
		t.Errorf("Turns[0] = %+v, want turn-1 output %q is_error false", turn, "hello\nworld")
	}
	if turn.EndedAt == nil {
		t.Errorf("Turns[0].EndedAt = nil, want a time")
	} else if want := mustParseTime(t, "2026-11-19T06:30:02.500Z"); !turn.EndedAt.Equal(want) {
		t.Errorf("Turns[0].EndedAt = %v, want %v", turn.EndedAt.Time, want)
	}
	if detail.Turns[1].EndedAt != nil {
		t.Errorf("Turns[1].EndedAt = %v, want nil", detail.Turns[1].EndedAt.Time)
	}
	if !detail.Turns[1].IsError {
		t.Error("Turns[1].IsError = false, want true")
	}
	if len(detail.Suspensions) != 2 {
		t.Fatalf("len(Suspensions) = %d, want 2", len(detail.Suspensions))
	}
	if s := detail.Suspensions[0]; s.SnapshotMS != nil || s.RestoreMS != nil || s.HelloMS != nil || s.Error != "" {
		t.Errorf("Suspensions[0] = %+v, want all millisecond fields nil and empty error", s)
	}
	s := detail.Suspensions[1]
	if s.SnapshotMS == nil || *s.SnapshotMS != 123 {
		t.Errorf("Suspensions[1].SnapshotMS = %v, want 123", s.SnapshotMS)
	}
	if s.RestoreMS == nil || *s.RestoreMS != 456 {
		t.Errorf("Suspensions[1].RestoreMS = %v, want 456", s.RestoreMS)
	}
	if s.HelloMS == nil || *s.HelloMS != 789 {
		t.Errorf("Suspensions[1].HelloMS = %v, want 789", s.HelloMS)
	}
	if s.Error != "restore failed" {
		t.Errorf("Suspensions[1].Error = %q, want %q", s.Error, "restore failed")
	}

	got := <-records
	if got.method != http.MethodGet {
		t.Errorf("method = %q, want %q", got.method, http.MethodGet)
	}
	if got.path != "/v1/runs/run-1" {
		t.Errorf("path = %q, want %q", got.path, "/v1/runs/run-1")
	}
}

func TestSend(t *testing.T) {
	t.Parallel()

	records := make(chan record, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"inbox_seq":7}`)
	})

	seq, err := c.Send(context.Background(), "run-1", "hello world")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if seq != 7 {
		t.Errorf("inboxSeq = %d, want 7", seq)
	}

	got := <-records
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want %q", got.method, http.MethodPost)
	}
	if got.path != "/v1/runs/run-1/messages" {
		t.Errorf("path = %q, want %q", got.path, "/v1/runs/run-1/messages")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got.contentType, "application/json")
	}
	if string(got.body) != `{"text":"hello world"}` {
		t.Errorf("body = %q, want %q", got.body, `{"text":"hello world"}`)
	}
}

func TestEnd(t *testing.T) {
	t.Parallel()

	records := make(chan record, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	if err := c.End(context.Background(), "run-1"); err != nil {
		t.Fatalf("End: %v", err)
	}

	got := <-records
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want %q", got.method, http.MethodPost)
	}
	if got.path != "/v1/runs/run-1/end" {
		t.Errorf("path = %q, want %q", got.path, "/v1/runs/run-1/end")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got.contentType, "application/json")
	}
	if len(got.body) != 0 {
		t.Errorf("body = %q, want empty", got.body)
	}
}

func TestEndNoContent(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.End(context.Background(), "run-1"); err != nil {
		t.Fatalf("End: %v", err)
	}
}

func TestPathEscaping(t *testing.T) {
	t.Parallel()

	const runID = "run/with space?and#hash"
	records := make(chan record, 3)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	ctx := context.Background()
	if _, err := c.Get(ctx, runID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Send(ctx, runID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.End(ctx, runID); err != nil {
		t.Fatalf("End: %v", err)
	}

	escapedID := url.PathEscape(runID)
	want := []string{
		"/v1/runs/" + escapedID,
		"/v1/runs/" + escapedID + "/messages",
		"/v1/runs/" + escapedID + "/end",
	}
	for i, w := range want {
		got := <-records
		if got.path != w {
			t.Errorf("request %d path = %q, want %q", i, got.path, w)
		}
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	t.Parallel()

	records := make(chan record, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	for _, baseURL := range []string{srv.URL, srv.URL + "/", srv.URL + "//"} {
		c, err := New(baseURL, nil)
		if err != nil {
			t.Fatalf("New(%q): %v", baseURL, err)
		}
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("List with base URL %q: %v", baseURL, err)
		}
	}
	for i := 0; i < 3; i++ {
		got := <-records
		if got.path != "/v1/runs" {
			t.Errorf("base URL %d path = %q, want %q", i, got.path, "/v1/runs")
		}
	}
}

func TestAPIErrorMessages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		status      int
		body        string
		wantStatus  int
		wantMessage string
	}{
		{"json 400", 400, `{"error":"text empty or longer than 64 KiB"}`, 400, "text empty or longer than 64 KiB"},
		{"json 404", 404, `{"error":"run not found"}`, 404, "run not found"},
		{"json 409", 409, `{"error":"run ended or failed"}`, 409, "run ended or failed"},
		{"json 500", 500, `{"error":"internal error"}`, 500, "internal error"},
		{"text 405", 405, "Method Not Allowed\n", 405, "Method Not Allowed"},
		{"empty 502", 502, "", 502, "Bad Gateway"},
		{"html 500", 500, "<html>oops</html>", 500, "Internal Server Error"},
		{"invalid json 503", 503, "{not json", 503, "Service Unavailable"},
		{"json without error", 400, `{}`, 400, "Bad Request"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			_, err := c.List(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", apiErr.Status, tc.wantStatus)
			}
			if apiErr.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, tc.wantMessage)
			}
			wantText := fmt.Sprintf("server returned %d: %s", tc.wantStatus, tc.wantMessage)
			if err.Error() != wantText {
				t.Errorf("Error() = %q, want %q", err.Error(), wantText)
			}
		})
	}
}

func TestAPIErrorEveryMethod(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"run not found"}`)
	})

	ctx := context.Background()
	calls := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, err := c.Create(ctx, "prompt"); return err }},
		{"List", func() error { _, err := c.List(ctx); return err }},
		{"Get", func() error { _, err := c.Get(ctx, "run-1"); return err }},
		{"Send", func() error { _, err := c.Send(ctx, "run-1", "text"); return err }},
		{"End", func() error { return c.End(ctx, "run-1") }},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			err := call.call()
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.Status != http.StatusNotFound {
				t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusNotFound)
			}
			if apiErr.Message != "run not found" {
				t.Errorf("Message = %q, want %q", apiErr.Message, "run not found")
			}
		})
	}
}

func TestTransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := srv.URL
	srv.Close()

	c, err := New(baseURL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.List(context.Background())
	if err == nil {
		t.Fatal("List = nil error, want a transport error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("error = %v (%T), want a transport error, not *APIError", err, err)
	}
}

func TestContextCanceledBeforeCall(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		_, _ = io.WriteString(w, `{}`)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Create(ctx, "prompt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v, want context.Canceled", err)
	}
	select {
	case <-called:
		t.Error("request reached the server after the context was canceled")
	default:
	}
}

func TestContextCanceledInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.List(ctx)
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the server")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("List error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("List did not return after the context was canceled")
	}
}

// TestNoRedirectFollow 断言客户端不跟随重定向，3xx 原样转成 *APIError。
func TestNoRedirectFollow(t *testing.T) {
	t.Parallel()

	var targetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(target.Close)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/runs", http.StatusFound)
	})

	_, err := c.List(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusFound {
		t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusFound)
	}
	if apiErr.Message != http.StatusText(http.StatusFound) {
		t.Errorf("Message = %q, want %q", apiErr.Message, http.StatusText(http.StatusFound))
	}
	if targetHit.Load() {
		t.Error("客户端跟随了重定向")
	}
}

// TestResponseBodyLimit 断言超过上限的响应体被拒绝，而不是整读进内存。
func TestResponseBodyLimit(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[")
		_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBytes))
		_, _ = io.WriteString(w, "]")
	})

	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("List = nil error, want a body-too-large error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want a non-APIError", err, err)
	}
	if !strings.Contains(err.Error(), "response body") {
		t.Errorf("error = %v, want it to mention the response body limit", err)
	}
}

func TestListEmpty(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})

	runs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("len(runs) = %d, want 0", len(runs))
	}
}
