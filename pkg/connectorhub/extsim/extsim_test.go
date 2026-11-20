package extsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var testSigningKey = []byte("extsim-test-signing-key")

// signBody 按 extsim 的规则计算签名头取值。
func signBody(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"job":{"id":"job-1","status":"completed","conclusion":"succeeded"}}`)
	otherBody := []byte(`{"job":{"id":"job-2","status":"completed","conclusion":"failed"}}`)

	tests := []struct {
		name   string
		header func() http.Header
		body   []byte
		key    []byte
		want   bool
	}{
		{
			name: "correct signature",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, signBody(testSigningKey, body))
				return h
			},
			body: body,
			key:  testSigningKey,
			want: true,
		},
		{
			name: "wrong signature",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, signBody([]byte("other-key"), body))
				return h
			},
			body: body,
			key:  testSigningKey,
			want: false,
		},
		{
			name: "signature for another body",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, signBody(testSigningKey, otherBody))
				return h
			},
			body: body,
			key:  testSigningKey,
			want: false,
		},
		{
			name:   "missing header",
			header: func() http.Header { return http.Header{} },
			body:   body,
			key:    testSigningKey,
			want:   false,
		},
		{
			name: "wrong prefix",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, "sha1="+hex.EncodeToString([]byte("0123456789012345678901234567890123456789")))
				return h
			},
			body: body,
			key:  testSigningKey,
			want: false,
		},
		{
			name: "malformed hex",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, signaturePrefix+"not-hex")
				return h
			},
			body: body,
			key:  testSigningKey,
			want: false,
		},
		{
			name: "digest of wrong length",
			header: func() http.Header {
				h := http.Header{}
				h.Set(HeaderSignature, signaturePrefix+"abcd")
				return h
			},
			body: body,
			key:  testSigningKey,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("http://127.0.0.1:1", nil)
			if got := c.VerifySignature(tc.header(), tc.body, tc.key); got != tc.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEventType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  http.Header
		want    string
		wantErr bool
	}{
		{
			name:   "job completed",
			header: http.Header{HeaderEvent: []string{EventJobCompleted}},
			want:   EventJobCompleted,
		},
		{
			name:    "missing header",
			header:  http.Header{},
			wantErr: true,
		},
		{
			name:    "empty header",
			header:  http.Header{HeaderEvent: []string{"  "}},
			wantErr: true,
		},
		{
			name:    "unknown event type",
			header:  http.Header{HeaderEvent: []string{"job.updated"}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("http://127.0.0.1:1", nil)
			got, err := c.EventType(tc.header)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("EventType = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("EventType 返回错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EventType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "succeeded",
			body: `{"job":{"id":"job-1","status":"completed","conclusion":"succeeded"}}`,
			want: "succeeded",
		},
		{
			name: "failed",
			body: `{"job":{"id":"job-1","status":"completed","conclusion":"failed","reason":"missing required field \"owner\""}}`,
			want: "failed",
		},
		{
			name:    "not json",
			body:    `not-json`,
			wantErr: true,
		},
		{
			name:    "missing job",
			body:    `{"event":"job.completed"}`,
			wantErr: true,
		},
		{
			name:    "missing conclusion",
			body:    `{"job":{"id":"job-1","status":"completed"}}`,
			wantErr: true,
		},
		{
			name:    "null conclusion",
			body:    `{"job":{"id":"job-1","status":"completed","conclusion":null}}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("http://127.0.0.1:1", nil)
			got, err := c.Terminal([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Terminal = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Terminal 返回错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Terminal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewDefaultStatusTimeout(t *testing.T) {
	t.Parallel()

	c := New("http://127.0.0.1:1", nil)
	if c.statusTimeout != 5*time.Second {
		t.Fatalf("statusTimeout = %v, want 5s", c.statusTimeout)
	}
}

func TestCheckStatusCompleted(t *testing.T) {
	t.Parallel()

	const jobID = "job-abcdefghijkl"
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + jobID + `","status":"completed","conclusion":"succeeded","reason":""}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL+"/", srv.Client())
	terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": jobID})
	if err != nil {
		t.Fatalf("CheckStatus 返回错误: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("请求方法 = %q, want GET", gotMethod)
	}
	if want := "/v1/jobs/" + jobID; gotPath != want {
		t.Fatalf("请求路径 = %q, want %q", gotPath, want)
	}
	if !reached {
		t.Fatal("reached = false, want true")
	}
	if terminal != "succeeded" {
		t.Fatalf("terminal = %q, want %q", terminal, "succeeded")
	}
	if len(body) == 0 {
		t.Fatal("body 为空，want 响应体原文")
	}
}

func TestCheckStatusNotCompleted(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"id":"job-1","status":"` + status + `"}`))
			}))
			t.Cleanup(srv.Close)

			c := New(srv.URL, srv.Client())
			terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": "job-1"})
			if err != nil {
				t.Fatalf("CheckStatus 返回错误: %v", err)
			}
			if reached {
				t.Fatal("reached = true, want false")
			}
			if terminal != "" {
				t.Fatalf("terminal = %q, want empty", terminal)
			}
			if len(body) == 0 {
				t.Fatal("body 为空，want 响应体原文")
			}
		})
	}
}

func TestCheckStatusEscapesJobID(t *testing.T) {
	t.Parallel()

	const (
		jobID       = "job/alpha?beta"
		wantRawPath = "/v1/jobs/job%2Falpha%3Fbeta"
	)
	var gotRawPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + jobID + `","status":"completed","conclusion":"succeeded"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client())
	terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": jobID})
	if err != nil {
		t.Fatalf("CheckStatus 返回错误: %v", err)
	}
	if gotRawPath != wantRawPath {
		t.Fatalf("请求路径 = %q, want %q", gotRawPath, wantRawPath)
	}
	if gotQuery != "" {
		t.Fatalf("RawQuery = %q, want empty", gotQuery)
	}
	if !reached {
		t.Fatal("reached = false, want true")
	}
	if terminal != "succeeded" {
		t.Fatalf("terminal = %q, want %q", terminal, "succeeded")
	}
	if len(body) == 0 {
		t.Fatal("body 为空，want 响应体原文")
	}
}

func TestCheckStatusNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client())
	terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": "job-missing"})
	if err == nil {
		t.Fatal("CheckStatus 返回 nil 错误，want 不可达错误")
	}
	if reached {
		t.Fatal("reached = true, want false")
	}
	if terminal != "" {
		t.Fatalf("terminal = %q, want empty", terminal)
	}
	if body != nil {
		t.Fatalf("body = %q, want nil", body)
	}
}

func TestCheckStatusTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client())
	c.statusTimeout = 50 * time.Millisecond
	terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": "job-slow"})
	if err == nil {
		t.Fatal("CheckStatus 返回 nil 错误，want 超时错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误 = %v, want context.DeadlineExceeded", err)
	}
	if reached {
		t.Fatal("reached = true, want false")
	}
	if terminal != "" {
		t.Fatalf("terminal = %q, want empty", terminal)
	}
	if body != nil {
		t.Fatalf("body = %q, want nil", body)
	}
}

func TestCheckStatusConnectionFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(url, &http.Client{})
	terminal, reached, body, err := c.CheckStatus(context.Background(), APIJob, map[string]string{"job.id": "job-1"})
	if err == nil {
		t.Fatal("CheckStatus 返回 nil 错误，want 连接失败错误")
	}
	if reached {
		t.Fatal("reached = true, want false")
	}
	if terminal != "" {
		t.Fatalf("terminal = %q, want empty", terminal)
	}
	if body != nil {
		t.Fatalf("body = %q, want nil", body)
	}
}

func TestCheckStatusRejectsBadArguments(t *testing.T) {
	t.Parallel()

	c := New("http://127.0.0.1:1", nil)

	if _, _, _, err := c.CheckStatus(context.Background(), "other-api", map[string]string{"job.id": "job-1"}); err == nil {
		t.Fatal("未知 api 返回 nil 错误")
	}
	if _, _, _, err := c.CheckStatus(context.Background(), APIJob, map[string]string{}); err == nil {
		t.Fatal("缺少 job.id 返回 nil 错误")
	}
}
