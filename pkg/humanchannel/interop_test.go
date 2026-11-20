package humanchannel

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/humanchannel/api"
	"github.com/wangzw/castworks/pkg/humanchannel/client"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

// newInteropClient 把 NewHandler(假 RunService) 挂到 httptest.Server，返回走
// client 包的客户端，成功后的清理注册到 t.Cleanup。
func newInteropClient(t *testing.T, svc runcontroller.RunService) *client.Client {
	t.Helper()
	srv := httptest.NewServer(NewHandler(svc))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

// wantTime 按时刻比较 api.Time 与期望值。
func wantTime(t *testing.T, name string, got api.Time, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %s，想要 %s", name, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// TestInteropSuccessRoundTrip 用 client 包走完五个调用，断言请求参数到达
// RunService、响应的数据与假 RunService 提供的值一致，覆盖 EndedAt 与三个
// 毫秒字段为 nil 与非 nil 两种情形。
func TestInteropSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 11, 19, 6, 30, 0, 123000000, time.UTC)
	older := created.Add(-time.Minute)
	turn1Start := time.Date(2026, 11, 19, 6, 31, 0, 0, time.UTC)
	turn2Start := time.Date(2026, 11, 19, 6, 33, 0, 0, time.UTC)
	turn2End := time.Date(2026, 11, 19, 6, 34, 0, 456000000, time.UTC)
	suspend1Start := time.Date(2026, 11, 19, 6, 35, 0, 0, time.UTC)
	suspend2Start := time.Date(2026, 11, 19, 6, 36, 0, 0, time.UTC)
	snapshotMS := int64(1200)
	restoreMS := int64(3400)
	helloMS := int64(56)

	svc := &FakeRunService{
		CreateFunc: func(_ context.Context, _ string) (string, error) {
			return "run-1", nil
		},
		ListFunc: func(_ context.Context) ([]runcontroller.RunSummary, error) {
			return []runcontroller.RunSummary{
				{RunID: "run-1", State: runcontroller.StateActive, Substate: runcontroller.SubstateInTurn, CreatedAt: created},
				{RunID: "run-2", State: runcontroller.StateSuspended, Substate: runcontroller.SubstateNone, CreatedAt: older},
			}, nil
		},
		GetFunc: func(_ context.Context, _ string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{
				RunSummary: runcontroller.RunSummary{
					RunID:     "run-1",
					State:     runcontroller.StateFailed,
					Substate:  runcontroller.SubstateNone,
					CreatedAt: created,
				},
				FailureReason: "provision timeout",
				Turns: []runcontroller.Turn{
					{TurnID: "t1", StartedAt: turn1Start, EndedAt: nil, Output: "hello\nworld\n", IsError: false},
					{TurnID: "t2", StartedAt: turn2Start, EndedAt: &turn2End, Output: "", IsError: true},
				},
				Suspensions: []runcontroller.Suspension{
					{StartedAt: suspend1Start, SnapshotMS: nil, RestoreMS: nil, HelloMS: nil, Error: ""},
					{StartedAt: suspend2Start, SnapshotMS: &snapshotMS, RestoreMS: &restoreMS, HelloMS: &helloMS, Error: "hello timeout"},
				},
			}, nil
		},
		AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
			return 42, false, nil
		},
		EndFunc: func(_ context.Context, _ string) error { return nil },
	}
	c := newInteropClient(t, svc)
	ctx := context.Background()

	runID, err := c.Create(ctx, "先读设计再动手")
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if runID != "run-1" {
		t.Fatalf("Create 返回的 run_id = %q，想要 %q", runID, "run-1")
	}
	creates := svc.CreateCalls()
	if len(creates) != 1 || creates[0].Prompt != "先读设计再动手" {
		t.Fatalf("Create 调用记录 = %+v", creates)
	}

	runs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("List 返回 %d 个运行实例，想要 2", len(runs))
	}
	if runs[0].RunID != "run-1" || runs[0].State != "active" || runs[0].Substate != "in_turn" {
		t.Fatalf("List 第一个运行实例 = %+v", runs[0])
	}
	wantTime(t, "List 第一个运行实例的 created_at", runs[0].CreatedAt, created)
	if runs[1].RunID != "run-2" || runs[1].State != "suspended" || runs[1].Substate != "" {
		t.Fatalf("List 第二个运行实例 = %+v", runs[1])
	}
	wantTime(t, "List 第二个运行实例的 created_at", runs[1].CreatedAt, older)
	if lists := svc.ListCalls(); len(lists) != 1 {
		t.Fatalf("List 调用次数 = %d，想要 1", len(lists))
	}

	// 带空格的 run_id 经客户端转义后应原样到达 RunService 与路径参数。
	escapedID := "run-1 with space"
	detail, err := c.Get(ctx, escapedID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if gets := svc.GetCalls(); len(gets) != 1 || gets[0].RunID != escapedID {
		t.Fatalf("Get 调用记录 = %+v，想要 run_id %q", gets, escapedID)
	}
	if detail.RunID != "run-1" || detail.State != "failed" || detail.Substate != "" {
		t.Fatalf("Get 的摘要 = %+v", detail.RunSummary)
	}
	wantTime(t, "Get 的 created_at", detail.CreatedAt, created)
	if detail.FailureReason != "provision timeout" {
		t.Fatalf("Get 的 failure_reason = %q", detail.FailureReason)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("Get 返回 %d 个回合，想要 2", len(detail.Turns))
	}
	if detail.Turns[0].TurnID != "t1" || detail.Turns[0].Output != "hello\nworld\n" || detail.Turns[0].IsError {
		t.Fatalf("第一个回合 = %+v", detail.Turns[0])
	}
	wantTime(t, "第一个回合的 started_at", detail.Turns[0].StartedAt, turn1Start)
	if detail.Turns[0].EndedAt != nil {
		t.Fatalf("第一个回合的 ended_at = %s，想要 null", detail.Turns[0].EndedAt.Format(time.RFC3339Nano))
	}
	if detail.Turns[1].TurnID != "t2" || !detail.Turns[1].IsError {
		t.Fatalf("第二个回合 = %+v", detail.Turns[1])
	}
	if detail.Turns[1].EndedAt == nil {
		t.Fatal("第二个回合的 ended_at 是 null，想要非空")
	}
	wantTime(t, "第二个回合的 started_at", detail.Turns[1].StartedAt, turn2Start)
	wantTime(t, "第二个回合的 ended_at", *detail.Turns[1].EndedAt, turn2End)
	if len(detail.Suspensions) != 2 {
		t.Fatalf("Get 返回 %d 条挂起记录，想要 2", len(detail.Suspensions))
	}
	first := detail.Suspensions[0]
	if first.SnapshotMS != nil || first.RestoreMS != nil || first.HelloMS != nil || first.Error != "" {
		t.Fatalf("第一条挂起记录 = %+v，想要三个毫秒字段为 null", first)
	}
	wantTime(t, "第一条挂起记录的 started_at", first.StartedAt, suspend1Start)
	second := detail.Suspensions[1]
	if second.SnapshotMS == nil || *second.SnapshotMS != snapshotMS {
		t.Fatalf("第二条挂起记录的 snapshot_ms = %v，想要 %d", second.SnapshotMS, snapshotMS)
	}
	if second.RestoreMS == nil || *second.RestoreMS != restoreMS {
		t.Fatalf("第二条挂起记录的 restore_ms = %v，想要 %d", second.RestoreMS, restoreMS)
	}
	if second.HelloMS == nil || *second.HelloMS != helloMS {
		t.Fatalf("第二条挂起记录的 hello_ms = %v，想要 %d", second.HelloMS, helloMS)
	}
	if second.Error != "hello timeout" {
		t.Fatalf("第二条挂起记录的 error = %q", second.Error)
	}
	wantTime(t, "第二条挂起记录的 started_at", second.StartedAt, suspend2Start)

	seq, err := c.Send(ctx, escapedID, "补充一句")
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if seq != 42 {
		t.Fatalf("Send 返回的 inbox_seq = %d，想要 42", seq)
	}
	appends := svc.AppendCalls()
	if len(appends) != 1 || appends[0].RunID != escapedID || appends[0].Text != "补充一句" {
		t.Fatalf("AppendInbox 调用记录 = %+v", appends)
	}

	if err := c.End(ctx, escapedID); err != nil {
		t.Fatalf("End 失败: %v", err)
	}
	ends := svc.EndCalls()
	if len(ends) != 1 || ends[0].RunID != escapedID {
		t.Fatalf("End 调用记录 = %+v", ends)
	}
}

// TestInteropRunIDWithSlash 断言 run_id 里的斜杠经客户端转义为 %2F 后，仍能
// 作为单个路径段到达处理器，并在 RunService 收到原始值。
func TestInteropRunIDWithSlash(t *testing.T) {
	t.Parallel()

	const runID = "run/with/slash"
	svc := &FakeRunService{
		GetFunc: func(_ context.Context, id string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{RunSummary: runcontroller.RunSummary{RunID: id}}, nil
		},
		AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
			return 1, false, nil
		},
		EndFunc: func(_ context.Context, _ string) error { return nil },
	}
	c := newInteropClient(t, svc)
	ctx := context.Background()

	detail, err := c.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if detail.RunID != runID {
		t.Fatalf("Get 返回的 run_id = %q，想要 %q", detail.RunID, runID)
	}
	if gets := svc.GetCalls(); len(gets) != 1 || gets[0].RunID != runID {
		t.Fatalf("Get 调用记录 = %+v，想要 run_id %q", gets, runID)
	}

	if _, err := c.Send(ctx, runID, "补充一句"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if appends := svc.AppendCalls(); len(appends) != 1 || appends[0].RunID != runID {
		t.Fatalf("AppendInbox 调用记录 = %+v，想要 run_id %q", appends, runID)
	}

	if err := c.End(ctx, runID); err != nil {
		t.Fatalf("End 失败: %v", err)
	}
	if ends := svc.EndCalls(); len(ends) != 1 || ends[0].RunID != runID {
		t.Fatalf("End 调用记录 = %+v，想要 run_id %q", ends, runID)
	}
}

// TestInteropEmptyCollections 断言空列表与空的 turns、suspensions 经客户端
// 解码后是长度为 0 的非 nil 切片，即线上是 [] 而不是 null。
func TestInteropEmptyCollections(t *testing.T) {
	t.Parallel()

	svc := &FakeRunService{
		ListFunc: func(_ context.Context) ([]runcontroller.RunSummary, error) {
			return nil, nil
		},
		GetFunc: func(_ context.Context, _ string) (runcontroller.RunDetail, error) {
			return runcontroller.RunDetail{
				RunSummary: runcontroller.RunSummary{RunID: "empty"},
			}, nil
		},
	}
	c := newInteropClient(t, svc)
	ctx := context.Background()

	runs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if runs == nil || len(runs) != 0 {
		t.Fatalf("List 返回 %#v，想要非 nil 的空切片", runs)
	}

	detail, err := c.Get(ctx, "empty")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if detail.Turns == nil || len(detail.Turns) != 0 {
		t.Fatalf("Get 的 turns = %#v，想要非 nil 的空切片", detail.Turns)
	}
	if detail.Suspensions == nil || len(detail.Suspensions) != 0 {
		t.Fatalf("Get 的 suspensions = %#v，想要非 nil 的空切片", detail.Suspensions)
	}
}

// TestInteropSentinelErrorMapping 断言三个哨兵错误经处理器映射成状态码后，
// 客户端拿到的是对应 Status 的 *client.APIError，Message 取处理器返回的固定文案。
func TestInteropSentinelErrorMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		svc        *FakeRunService
		call       func(context.Context, *client.Client) error
		wantStatus int
		wantText   string
	}{
		{
			name: "ErrInvalidEvent 映射 400",
			svc: &FakeRunService{
				CreateFunc: func(_ context.Context, _ string) (string, error) {
					return "", runcontroller.ErrInvalidEvent
				},
			},
			call: func(ctx context.Context, c *client.Client) error {
				_, err := c.Create(ctx, "x")
				return err
			},
			wantStatus: 400,
			wantText:   "text empty or longer than 64 KiB",
		},
		{
			name: "ErrNotFound 映射 404",
			svc: &FakeRunService{
				GetFunc: func(_ context.Context, _ string) (runcontroller.RunDetail, error) {
					return runcontroller.RunDetail{}, runcontroller.ErrNotFound
				},
			},
			call: func(ctx context.Context, c *client.Client) error {
				_, err := c.Get(ctx, "missing")
				return err
			},
			wantStatus: 404,
			wantText:   "run not found",
		},
		{
			name: "ErrTerminated 映射 409",
			svc: &FakeRunService{
				AppendInboxFunc: func(_ context.Context, _ string, _ runcontroller.InboxEvent) (int64, bool, error) {
					return 0, false, runcontroller.ErrTerminated
				},
			},
			call: func(ctx context.Context, c *client.Client) error {
				_, err := c.Send(ctx, "ended", "补充")
				return err
			},
			wantStatus: 409,
			wantText:   "run ended or failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newInteropClient(t, tc.svc)

			err := tc.call(context.Background(), c)
			if err == nil {
				t.Fatal("想要错误，得到 nil")
			}
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("错误 %v 不是 *client.APIError", err)
			}
			if apiErr.Status != tc.wantStatus {
				t.Fatalf("APIError.Status = %d，想要 %d", apiErr.Status, tc.wantStatus)
			}
			if apiErr.Message != tc.wantText {
				t.Fatalf("APIError.Message = %q，想要 %q", apiErr.Message, tc.wantText)
			}
		})
	}
}
