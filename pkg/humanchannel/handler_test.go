package humanchannel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/humanchannel/api"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

func newTestServer(t *testing.T, svc runcontroller.RunService) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(svc))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string) (status int, header http.Header, respBody []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, resp.Header, b
}

func wantContentType(t *testing.T, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q，想要 %q", got, "application/json")
	}
}

// totalCalls 返回假 RunService 收到的调用总次数。
func totalCalls(svc *FakeRunService) int {
	return len(svc.CreateCalls()) + len(svc.AppendCalls()) + len(svc.GetCalls()) +
		len(svc.ListCalls()) + len(svc.EndCalls())
}

// doHandler 把一次请求直接交给 h 处理，返回状态码、响应头与响应体。
func doHandler(t *testing.T, h http.Handler, req *http.Request) (status int, header http.Header, respBody []byte) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Header(), rr.Body.Bytes()
}

func TestCreateSuccess(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		CreateFunc: func(_ context.Context, prompt string) (string, error) {
			return "run-abc", nil
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodPost, srv.URL+"/v1/runs", `{"prompt":"先读设计再动手"}`)
	if status != http.StatusCreated {
		t.Fatalf("状态码 = %d，想要 201，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	if got, want := string(body), "{\"run_id\":\"run-abc\"}\n"; got != want {
		t.Fatalf("响应体 = %q，想要 %q", got, want)
	}

	calls := svc.CreateCalls()
	if len(calls) != 1 {
		t.Fatalf("Create 调用次数 = %d，想要 1", len(calls))
	}
	if calls[0].Prompt != "先读设计再动手" {
		t.Fatalf("Create 收到的 prompt = %q", calls[0].Prompt)
	}
	if calls[0].Ctx == nil {
		t.Fatal("Create 没有收到 ctx")
	}
}

func TestCreateWhitespacePromptAccepted(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		CreateFunc: func(_ context.Context, _ string) (string, error) { return "run-1", nil },
	}
	srv := newTestServer(t, svc)

	status, _, body := do(t, http.MethodPost, srv.URL+"/v1/runs", `{"prompt":"   "}`)
	if status != http.StatusCreated {
		t.Fatalf("状态码 = %d，想要 201，响应体 %s", status, body)
	}
	calls := svc.CreateCalls()
	if len(calls) != 1 || calls[0].Prompt != "   " {
		t.Fatalf("只含空白的 prompt 应交给 svc，调用记录 = %+v", calls)
	}
}

func TestListSuccess(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 11, 19, 6, 30, 0, 123456789, time.UTC)
	svc := &FakeRunService{
		ListFunc: func(_ context.Context) ([]runcontroller.RunSummary, error) {
			return []runcontroller.RunSummary{
				{RunID: "r1", State: runcontroller.StateActive, Substate: runcontroller.SubstateNone, CreatedAt: created},
				{RunID: "r2", State: runcontroller.StateSuspended, Substate: runcontroller.SubstateSnapshotted, CreatedAt: created.Add(-time.Minute)},
			}, nil
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodGet, srv.URL+"/v1/runs", "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, body)
	}
	wantContentType(t, hdr)

	const wantBody = `[{"run_id":"r1","state":"active","substate":"","created_at":"2026-11-19T06:30:00.123Z"},{"run_id":"r2","state":"suspended","substate":"snapshotted","created_at":"2026-11-19T06:29:00.123Z"}]` + "\n"
	if got := string(body); got != wantBody {
		t.Fatalf("响应体 = %q，想要 %q", got, wantBody)
	}

	var got []api.RunSummary
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解码响应失败: %v，响应体 %s", err, body)
	}
	if len(got) != 2 {
		t.Fatalf("运行实例数 = %d，想要 2", len(got))
	}
	if got[0].RunID != "r1" || got[0].State != "active" || got[0].Substate != "" {
		t.Fatalf("第一条 summary = %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(created.Truncate(time.Millisecond)) {
		t.Fatalf("created_at = %v，想要 %v", got[0].CreatedAt.Time, created.Truncate(time.Millisecond))
	}
	if got[1].RunID != "r2" || got[1].State != "suspended" || got[1].Substate != "snapshotted" {
		t.Fatalf("第二条 summary = %+v", got[1])
	}
	if !strings.Contains(string(body), `"substate":""`) {
		t.Fatalf("空 Substate 应输出空字符串，响应体 %s", body)
	}
	if calls := svc.ListCalls(); len(calls) != 1 || calls[0].Ctx == nil {
		t.Fatalf("List 调用记录 = %+v", calls)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		ListFunc: func(_ context.Context) ([]runcontroller.RunSummary, error) { return nil, nil },
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodGet, srv.URL+"/v1/runs", "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200", status)
	}
	wantContentType(t, hdr)
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Fatalf("空列表响应体 = %q，想要 []", got)
	}
}

func TestGetSuccess(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC)
	started := created.Add(time.Second)
	ended := started.Add(2 * time.Second)
	snapshotMS := int64(12)
	helloMS := int64(7)
	svc := &FakeRunService{
		GetFunc: func(_ context.Context, runID string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{
				RunSummary: runcontroller.RunSummary{
					RunID: "r1", State: runcontroller.StateFailed,
					Substate: runcontroller.SubstateNone, CreatedAt: created,
				},
				FailureReason: "actor crashed",
				Turns: []runcontroller.Turn{
					{TurnID: "t1", StartedAt: started, EndedAt: &ended, Output: "hello\nworld", IsError: false},
					{TurnID: "t2", StartedAt: started.Add(3 * time.Second), EndedAt: nil, Output: "", IsError: true},
				},
				Suspensions: []runcontroller.Suspension{
					{StartedAt: started, SnapshotMS: &snapshotMS, RestoreMS: nil, HelloMS: &helloMS, Error: ""},
				},
			}, nil
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodGet, srv.URL+"/v1/runs/r1", "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, body)
	}
	wantContentType(t, hdr)

	const wantBody = `{"run_id":"r1","state":"failed","substate":"","created_at":"2026-11-19T06:30:00.000Z","failure_reason":"actor crashed","turns":[{"turn_id":"t1","started_at":"2026-11-19T06:30:01.000Z","ended_at":"2026-11-19T06:30:03.000Z","output":"hello\nworld","is_error":false},{"turn_id":"t2","started_at":"2026-11-19T06:30:04.000Z","ended_at":null,"output":"","is_error":true}],"suspensions":[{"started_at":"2026-11-19T06:30:01.000Z","snapshot_ms":12,"restore_ms":null,"hello_ms":7,"error":""}]}` + "\n"
	if got := string(body); got != wantBody {
		t.Fatalf("响应体 = %q，想要 %q", got, wantBody)
	}

	var got api.RunDetail
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解码响应失败: %v，响应体 %s", err, body)
	}
	if got.RunID != "r1" || got.State != "failed" || got.Substate != "" || got.FailureReason != "actor crashed" {
		t.Fatalf("detail 字段 = %+v", got.RunSummary)
	}
	if len(got.Turns) != 2 {
		t.Fatalf("回合数 = %d，想要 2", len(got.Turns))
	}
	if got.Turns[0].TurnID != "t1" || got.Turns[0].Output != "hello\nworld" || got.Turns[0].IsError {
		t.Fatalf("第一个回合 = %+v", got.Turns[0])
	}
	if got.Turns[0].EndedAt == nil || !got.Turns[0].EndedAt.Equal(ended) {
		t.Fatalf("第一个回合的 ended_at = %+v", got.Turns[0].EndedAt)
	}
	if got.Turns[1].EndedAt != nil {
		t.Fatalf("未结束回合的 ended_at 应为 null，得到 %+v", got.Turns[1].EndedAt)
	}
	if !strings.Contains(string(body), `"ended_at":null`) {
		t.Fatalf("未结束回合应输出 ended_at:null，响应体 %s", body)
	}
	if len(got.Suspensions) != 1 {
		t.Fatalf("挂起记录数 = %d，想要 1", len(got.Suspensions))
	}
	if got.Suspensions[0].SnapshotMS == nil || *got.Suspensions[0].SnapshotMS != snapshotMS {
		t.Fatalf("snapshot_ms = %+v", got.Suspensions[0].SnapshotMS)
	}
	if got.Suspensions[0].RestoreMS != nil {
		t.Fatalf("未完成的 restore_ms 应为 null，得到 %+v", got.Suspensions[0].RestoreMS)
	}
	if got.Suspensions[0].HelloMS == nil || *got.Suspensions[0].HelloMS != helloMS {
		t.Fatalf("hello_ms = %+v", got.Suspensions[0].HelloMS)
	}
	if calls := svc.GetCalls(); len(calls) != 1 || calls[0].RunID != "r1" {
		t.Fatalf("Get 调用记录 = %+v", calls)
	}
}

func TestGetNilSlicesEncodedAsEmptyArrays(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		GetFunc: func(_ context.Context, _ string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{RunSummary: runcontroller.RunSummary{RunID: "r1"}}, nil
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodGet, srv.URL+"/v1/runs/r1", "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200", status)
	}
	wantContentType(t, hdr)
	text := string(body)
	if !strings.Contains(text, `"turns":[]`) {
		t.Fatalf("nil Turns 应输出 []，响应体 %s", body)
	}
	if !strings.Contains(text, `"suspensions":[]`) {
		t.Fatalf("nil Suspensions 应输出 []，响应体 %s", body)
	}
	if !strings.Contains(text, `"failure_reason":""`) {
		t.Fatalf("空 failure_reason 应输出空字符串，响应体 %s", body)
	}
}

func TestSendSuccess(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
			return 7, false, nil
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/messages", `{"text":"你好"}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	if got, want := string(body), "{\"inbox_seq\":7}\n"; got != want {
		t.Fatalf("响应体 = %q，想要 %q", got, want)
	}
	calls := svc.AppendCalls()
	if len(calls) != 1 || calls[0].RunID != "r1" || calls[0].Text != "你好" {
		t.Fatalf("AppendInbox 调用记录 = %+v", calls)
	}
	if calls[0].Event.Kind != runcontroller.KindHumanMessage {
		t.Fatalf("AppendInbox 事件种类 = %q, 想要 human_message", calls[0].Event.Kind)
	}
}

func TestEndSuccessIgnoresBody(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/end", "这不是 JSON，处理器应忽略")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	if got, want := string(body), "{}\n"; got != want {
		t.Fatalf("响应体 = %q，想要 %q", got, want)
	}
	calls := svc.EndCalls()
	if len(calls) != 1 || calls[0].RunID != "r1" {
		t.Fatalf("End 调用记录 = %+v", calls)
	}
}

func TestGetAndEndIgnoreRequestBody(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{}
	srv := newTestServer(t, svc)

	if status, _, body := do(t, http.MethodGet, srv.URL+"/v1/runs/r1", "垃圾"); status != http.StatusOK {
		t.Fatalf("GET 请求体应被忽略，状态码 = %d，响应体 %s", status, body)
	}
	if status, _, body := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/end", "垃圾"); status != http.StatusOK {
		t.Fatalf("end 请求体应被忽略，状态码 = %d，响应体 %s", status, body)
	}
}

func TestRequestValidationRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   string
	}{
		{"create 缺字段", http.MethodPost, "/v1/runs", `{}`, "text empty or longer than 64 KiB"},
		{"create 空 prompt", http.MethodPost, "/v1/runs", `{"prompt":""}`, "text empty or longer than 64 KiB"},
		{"create 空请求体", http.MethodPost, "/v1/runs", "", "invalid request body"},
		{"create 非法 JSON", http.MethodPost, "/v1/runs", `not json`, "invalid request body"},
		{"create 未知字段", http.MethodPost, "/v1/runs", `{"prompt":"hi","extra":1}`, "invalid request body"},
		{"create 多余内容", http.MethodPost, "/v1/runs", `{"prompt":"a"}{"prompt":"b"}`, "invalid request body"},
		{"send 缺字段", http.MethodPost, "/v1/runs/r1/messages", `{}`, "text empty or longer than 64 KiB"},
		{"send 空 text", http.MethodPost, "/v1/runs/r1/messages", `{"text":""}`, "text empty or longer than 64 KiB"},
		{"send 空请求体", http.MethodPost, "/v1/runs/r1/messages", "", "invalid request body"},
		{"send 非法 JSON", http.MethodPost, "/v1/runs/r1/messages", `{`, "invalid request body"},
		{"send 未知字段", http.MethodPost, "/v1/runs/r1/messages", `{"text":"hi","x":true}`, "invalid request body"},
		{"send 多余内容", http.MethodPost, "/v1/runs/r1/messages", `{"text":"a"} {}`, "invalid request body"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &FakeRunService{}
			srv := newTestServer(t, svc)

			status, hdr, body := do(t, tc.method, srv.URL+tc.path, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, body)
			}
			wantContentType(t, hdr)

			var resp api.ErrorResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
			}
			if resp.Error != tc.want {
				t.Fatalf("error = %q，想要 %q", resp.Error, tc.want)
			}
			if n := len(svc.CreateCalls()) + len(svc.AppendCalls()) + len(svc.GetCalls()) + len(svc.ListCalls()) + len(svc.EndCalls()); n != 0 {
				t.Fatalf("校验失败时不应调用 RunService，调用次数 = %d", n)
			}
		})
	}
}

func TestTextSizeBoundary(t *testing.T) {
	t.Parallel()
	exact := strings.Repeat("a", runcontroller.MaxTextBytes)
	over := exact + "a"
	// 多字节文本用来区分按字节与按字符计数：汉字在 UTF-8 中占 3 字节。
	multiOver := strings.Repeat("汉", 21846)        // 65538 字节
	multiExact := strings.Repeat("汉", 21845) + "a" // 65536 字节
	if len(multiOver) != 65538 || len(multiExact) != 65536 {
		t.Fatalf("多字节测试数据的字节数不对: %d, %d", len(multiOver), len(multiExact))
	}

	t.Run("create 恰好上限通过", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{CreateFunc: func(_ context.Context, _ string) (string, error) { return "r1", nil }}
		srv := newTestServer(t, svc)
		body := `{"prompt":"` + exact + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs", body)
		if status != http.StatusCreated {
			t.Fatalf("状态码 = %d，想要 201，响应体 %s", status, respBody)
		}
		if calls := svc.CreateCalls(); len(calls) != 1 || len(calls[0].Prompt) != runcontroller.MaxTextBytes {
			t.Fatalf("Create 调用记录长度 = %d", len(calls))
		}
	})

	t.Run("create 多一字节被拒", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{}
		srv := newTestServer(t, svc)
		body := `{"prompt":"` + over + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs", body)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, respBody)
		}
		if len(svc.CreateCalls()) != 0 {
			t.Fatal("超限时不应调用 svc")
		}
	})

	t.Run("send 恰好上限通过", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
			return 1, false, nil
		}}
		srv := newTestServer(t, svc)
		body := `{"text":"` + exact + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/messages", body)
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, respBody)
		}
	})

	t.Run("send 多一字节被拒", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{}
		srv := newTestServer(t, svc)
		body := `{"text":"` + over + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/messages", body)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, respBody)
		}
		if len(svc.AppendCalls()) != 0 {
			t.Fatal("超限时不应调用 svc")
		}
	})

	t.Run("create 多字节超限被拒", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{}
		srv := newTestServer(t, svc)
		body := `{"prompt":"` + multiOver + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs", body)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, respBody)
		}
		if len(svc.CreateCalls()) != 0 {
			t.Fatal("超限时不应调用 svc")
		}
	})

	t.Run("send 多字节超限被拒", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{}
		srv := newTestServer(t, svc)
		body := `{"text":"` + multiOver + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/messages", body)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, respBody)
		}
		if len(svc.AppendCalls()) != 0 {
			t.Fatal("超限时不应调用 svc")
		}
	})

	t.Run("create 多字节恰好上限通过", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{CreateFunc: func(_ context.Context, _ string) (string, error) { return "r1", nil }}
		srv := newTestServer(t, svc)
		body := `{"prompt":"` + multiExact + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs", body)
		if status != http.StatusCreated {
			t.Fatalf("状态码 = %d，想要 201，响应体 %s", status, respBody)
		}
		if calls := svc.CreateCalls(); len(calls) != 1 || len(calls[0].Prompt) != runcontroller.MaxTextBytes {
			t.Fatalf("Create 调用记录 = %+v", calls)
		}
	})

	t.Run("send 多字节恰好上限通过", func(t *testing.T) {
		t.Parallel()
		svc := &FakeRunService{AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
			return 1, false, nil
		}}
		srv := newTestServer(t, svc)
		body := `{"text":"` + multiExact + `"}`
		status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/messages", body)
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, respBody)
		}
	})
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{}
	srv := newTestServer(t, svc)

	// 合法 JSON 后跟超过 1 MiB 的空白，触发 MaxBytesReader。
	body := `{"prompt":"hi"}` + strings.Repeat(" ", 1<<20)
	status, _, respBody := do(t, http.MethodPost, srv.URL+"/v1/runs", body)
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，想要 400，响应体 %s", status, respBody)
	}
	if len(svc.CreateCalls()) != 0 {
		t.Fatal("请求体超限时不应调用 svc")
	}
}

func TestServiceErrorMapping(t *testing.T) {
	t.Parallel()
	sentinel := map[string]struct {
		err     error
		status  int
		message string
	}{
		"not found":     {runcontroller.ErrNotFound, http.StatusNotFound, "run not found"},
		"terminated":    {runcontroller.ErrTerminated, http.StatusConflict, "run ended or failed"},
		"invalid event": {runcontroller.ErrInvalidEvent, http.StatusBadRequest, "text empty or longer than 64 KiB"},
	}

	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		sentinel string
		wrapped  bool
	}{
		{"create invalid event", http.MethodPost, "/v1/runs", `{"prompt":"hi"}`, "invalid event", false},
		{"create wrapped invalid event", http.MethodPost, "/v1/runs", `{"prompt":"hi"}`, "invalid event", true},
		{"send invalid event", http.MethodPost, "/v1/runs/r1/messages", `{"text":"hi"}`, "invalid event", false},
		{"get not found", http.MethodGet, "/v1/runs/r1", "", "not found", false},
		{"get wrapped not found", http.MethodGet, "/v1/runs/r1", "", "not found", true},
		{"send not found", http.MethodPost, "/v1/runs/r1/messages", `{"text":"hi"}`, "not found", false},
		{"end not found", http.MethodPost, "/v1/runs/r1/end", "", "not found", false},
		{"send terminated", http.MethodPost, "/v1/runs/r1/messages", `{"text":"hi"}`, "terminated", false},
		{"send wrapped terminated", http.MethodPost, "/v1/runs/r1/messages", `{"text":"hi"}`, "terminated", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := sentinel[tc.sentinel].err
			err := base
			if tc.wrapped {
				err = fmt.Errorf("ctx: %w", base)
			}
			svc := &FakeRunService{
				CreateFunc:      func(context.Context, string) (string, error) { return "", err },
				GetFunc:         func(context.Context, string) (runcontroller.RunDetail, error) { return runcontroller.RunDetail{}, err },
				ListFunc:        func(context.Context) ([]runcontroller.RunSummary, error) { return nil, err },
				EndFunc:         func(context.Context, string) error { return err },
				AppendInboxFunc: func(context.Context, string, runcontroller.InboxEvent) (int64, bool, error) { return 0, false, err },
			}
			srv := newTestServer(t, svc)

			status, hdr, body := do(t, tc.method, srv.URL+tc.path, tc.body)
			if status != sentinel[tc.sentinel].status {
				t.Fatalf("状态码 = %d，想要 %d，响应体 %s", status, sentinel[tc.sentinel].status, body)
			}
			wantContentType(t, hdr)
			var resp api.ErrorResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
			}
			if want := sentinel[tc.sentinel].message; resp.Error != want {
				t.Fatalf("error = %q，想要 %q", resp.Error, want)
			}
		})
	}
}

func TestEndIdempotentReturnsOK(t *testing.T) {
	t.Parallel()
	// svc.End 对已结束的运行实例返回 nil，处理器仍应返回 200。
	svc := &FakeRunService{EndFunc: func(context.Context, string) error { return nil }}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodPost, srv.URL+"/v1/runs/r1/end", "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	if got, want := string(body), "{}\n"; got != want {
		t.Fatalf("响应体 = %q，想要 %q", got, want)
	}
}

func TestInternalErrorDoesNotLeak(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		GetFunc: func(context.Context, string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{}, errors.New("secret database detail")
		},
	}
	srv := newTestServer(t, svc)

	status, hdr, body := do(t, http.MethodGet, srv.URL+"/v1/runs/r1", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，想要 500，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	if strings.Contains(string(body), "secret database detail") {
		t.Fatalf("响应体泄露了内部错误文本: %s", body)
	}
	var resp api.ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
	}
	if resp.Error != "internal error" {
		t.Fatalf("error = %q，想要 internal error", resp.Error)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{}
	srv := newTestServer(t, svc)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodDelete, "/v1/runs"},
		{http.MethodPost, "/v1/runs/r1"},
		{http.MethodGet, "/v1/runs/r1/messages"},
		{http.MethodGet, "/v1/runs/r1/end"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			status, _, body := do(t, tc.method, srv.URL+tc.path, "")
			if status != http.StatusMethodNotAllowed {
				t.Fatalf("状态码 = %d，想要 405，响应体 %s", status, body)
			}
		})
	}
}

func TestUnknownPathNotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &FakeRunService{})
	status, _, _ := do(t, http.MethodGet, srv.URL+"/v1/unknown", "")
	if status != http.StatusNotFound {
		t.Fatalf("状态码 = %d，想要 404", status)
	}
}

// TestPostRequiresJSONContentType 断言每个 POST 路径都在 Content-Type 不是
// application/json 时返回 415，且不调用 RunService。Host 都是回环地址，只违反
// Content-Type 这一条规则。
func TestPostRequiresJSONContentType(t *testing.T) {
	t.Parallel()

	paths := []string{"/v1/runs", "/v1/runs/r1/messages", "/v1/runs/r1/end"}
	contentTypes := []struct {
		name  string
		value string
	}{
		{name: "缺席", value: ""},
		{name: "纯文本", value: "text/plain"},
		{name: "表单", value: "application/x-www-form-urlencoded"},
		{name: "相近类型", value: "application/jsonx"},
	}
	for _, path := range paths {
		for _, ct := range contentTypes {
			path, ct := path, ct
			t.Run(path+" "+ct.name, func(t *testing.T) {
				t.Parallel()
				svc := &FakeRunService{}
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prompt":"x","text":"x"}`))
				req.Host = "127.0.0.1"
				if ct.value != "" {
					req.Header.Set("Content-Type", ct.value)
				}
				status, hdr, body := doHandler(t, NewHandler(svc), req)
				if status != http.StatusUnsupportedMediaType {
					t.Fatalf("状态码 = %d，想要 415，响应体 %s", status, body)
				}
				wantContentType(t, hdr)
				var resp api.ErrorResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
				}
				if resp.Error != "unsupported media type" {
					t.Fatalf("error = %q，想要 %q", resp.Error, "unsupported media type")
				}
				if n := totalCalls(svc); n != 0 {
					t.Fatalf("Content-Type 不对时不应调用 RunService，调用次数 = %d", n)
				}
			})
		}
	}
}

func TestJSONContentTypeWithCharsetAccepted(t *testing.T) {
	t.Parallel()
	svc := &FakeRunService{
		CreateFunc: func(context.Context, string) (string, error) { return "r1", nil },
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"prompt":"hi"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	status, _, body := doHandler(t, NewHandler(svc), req)
	if status != http.StatusCreated {
		t.Fatalf("状态码 = %d，想要 201，响应体 %s", status, body)
	}
}

// TestHostMustBeLoopback 断言 Host 只有是回环地址才放行。拒绝的用例使用合法
// 的 Content-Type，只违反 Host 这一条规则。
func TestHostMustBeLoopback(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"127.0.0.1",
		"127.0.0.1:8080",
		"localhost",
		"localhost:8080",
		"LOCALHOST:1",
		"[::1]",
		"[::1]:8080",
	}
	for _, host := range allowed {
		host := host
		t.Run("允许 "+host, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
			req.Host = host
			status, _, body := doHandler(t, NewHandler(&FakeRunService{}), req)
			if status != http.StatusOK {
				t.Fatalf("Host %q: 状态码 = %d，想要 200，响应体 %s", host, status, body)
			}
		})
	}

	denied := []string{
		"",
		"example.com",
		"example.com:8080",
		"192.0.2.1",
		"10.0.0.1:8080",
		"127.0.0.2",
		"evil.localhost",
		"localhost.evil.com",
	}
	for _, host := range denied {
		host := host
		t.Run("拒绝 "+host, func(t *testing.T) {
			t.Parallel()
			svc := &FakeRunService{}

			// GET 也只违反 Host 一条规则。
			req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
			req.Host = host
			status, hdr, body := doHandler(t, NewHandler(svc), req)
			if status != http.StatusForbidden {
				t.Fatalf("Host %q: 状态码 = %d，想要 403，响应体 %s", host, status, body)
			}
			wantContentType(t, hdr)
			var resp api.ErrorResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
			}
			if resp.Error != "forbidden host" {
				t.Fatalf("error = %q，想要 %q", resp.Error, "forbidden host")
			}

			// POST 带合法 Content-Type，同样只因 Host 被拒。
			postReq := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"prompt":"hi"}`))
			postReq.Host = host
			postReq.Header.Set("Content-Type", "application/json")
			if status, _, body := doHandler(t, NewHandler(svc), postReq); status != http.StatusForbidden {
				t.Fatalf("Host %q 的 POST: 状态码 = %d，想要 403，响应体 %s", host, status, body)
			}

			if n := totalCalls(svc); n != 0 {
				t.Fatalf("Host 不合法时不应调用 RunService，调用次数 = %d", n)
			}
		})
	}
}

// TestGuardRunsBeforeRouting 断言 Host 检查在路由之前：未知路径与错误方法
// 在 Host 不是回环地址时也要先返回 403，而不是回落到 404 或 405。
func TestGuardRunsBeforeRouting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"未知路径", http.MethodGet, "/v1/unknown"},
		{"错误方法", http.MethodDelete, "/v1/runs"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &FakeRunService{}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = "example.com"
			status, hdr, body := doHandler(t, NewHandler(svc), req)
			if status != http.StatusForbidden {
				t.Fatalf("状态码 = %d，想要 403，响应体 %s", status, body)
			}
			wantContentType(t, hdr)
			var resp api.ErrorResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
			}
			if resp.Error != "forbidden host" {
				t.Fatalf("error = %q，想要 %q", resp.Error, "forbidden host")
			}
			if n := totalCalls(svc); n != 0 {
				t.Fatalf("Host 不合法时不应调用 RunService，调用次数 = %d", n)
			}
		})
	}
}

// TestGuardHostBeforeContentType 断言 Host 与 Content-Type 同时违规时先返回
// 403，而不是 415。
func TestGuardHostBeforeContentType(t *testing.T) {
	t.Parallel()

	svc := &FakeRunService{}
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"prompt":"hi"}`))
	req.Host = "example.com"
	req.Header.Set("Content-Type", "text/plain")
	status, hdr, body := doHandler(t, NewHandler(svc), req)
	if status != http.StatusForbidden {
		t.Fatalf("状态码 = %d，想要 403，响应体 %s", status, body)
	}
	wantContentType(t, hdr)
	var resp api.ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v，响应体 %s", err, body)
	}
	if resp.Error != "forbidden host" {
		t.Fatalf("error = %q，想要 %q", resp.Error, "forbidden host")
	}
	if n := totalCalls(svc); n != 0 {
		t.Fatalf("两项都违规时不应调用 RunService，调用次数 = %d", n)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	type cancellationCase struct {
		name   string
		method string
		path   string
		body   string
		setup  func(*FakeRunService, func(context.Context))
	}
	cases := []cancellationCase{
		{
			name: "create", method: http.MethodPost, path: "/v1/runs", body: `{"prompt":"x"}`,
			setup: func(f *FakeRunService, observe func(context.Context)) {
				f.CreateFunc = func(ctx context.Context, _ string) (string, error) { observe(ctx); return "r1", nil }
			},
		},
		{
			name: "list", method: http.MethodGet, path: "/v1/runs",
			setup: func(f *FakeRunService, observe func(context.Context)) {
				f.ListFunc = func(ctx context.Context) ([]runcontroller.RunSummary, error) { observe(ctx); return nil, nil }
			},
		},
		{
			name: "get", method: http.MethodGet, path: "/v1/runs/r1",
			setup: func(f *FakeRunService, observe func(context.Context)) {
				f.GetFunc = func(ctx context.Context, _ string) (runcontroller.RunDetail, error) {
					observe(ctx)
					return runcontroller.RunDetail{}, nil
				}
			},
		},
		{
			name: "messages", method: http.MethodPost, path: "/v1/runs/r1/messages", body: `{"text":"x"}`,
			setup: func(f *FakeRunService, observe func(context.Context)) {
				f.AppendInboxFunc = func(ctx context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
					observe(ctx)
					return 1, false, nil
				}
			},
		},
		{
			name: "end", method: http.MethodPost, path: "/v1/runs/r1/end",
			setup: func(f *FakeRunService, observe func(context.Context)) {
				f.EndFunc = func(ctx context.Context, _ string) error { observe(ctx); return nil }
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entered := make(chan struct{})
			observed := make(chan struct{})
			observe := func(ctx context.Context) {
				close(entered)
				// 同时等超时：ctx 没有被透传时测试会报错而不是一直挂住。
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
					t.Error("RunService 收到的 ctx 没有随请求取消")
				}
				close(observed)
			}
			svc := &FakeRunService{}
			tc.setup(svc, observe)
			srv := newTestServer(t, svc)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var rdr io.Reader
			if tc.body != "" {
				rdr = strings.NewReader(tc.body)
			}
			req, err := http.NewRequestWithContext(ctx, tc.method, srv.URL+tc.path, rdr)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("处理器没有把请求转交给 RunService")
			}
			cancel()
			select {
			case <-observed:
			case <-time.After(5 * time.Second):
				t.Fatal("RunService 收到的 ctx 没有随请求取消")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("取消后请求没有返回")
			}
		})
	}
}
