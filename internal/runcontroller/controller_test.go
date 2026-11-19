package runcontroller_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
	"github.com/wangzw/castworks/internal/runcontroller"
	"github.com/wangzw/castworks/internal/sandboxprovider"
	"github.com/wangzw/castworks/internal/sandboxprovider/fakeprovider"
	"github.com/wangzw/castworks/internal/testutil/fakeclock"
	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

var testEpoch = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// testTimeout 是等待异步事件落到数据库的真实时间预算。它只用于判断“卡死”，
// 不给正常延迟设上限。加压门禁（-count=10 -parallel=16）下 pgtest 为每个测试
// 建库删库，PostgreSQL 会因此执行强制 checkpoint；实测单次写盘可长达 14 秒，
// 期间整个服务器被拖慢，5 秒的预算会让状态等待偶发超时。
const testTimeout = 60 * time.Second

func testCredential() runcontroller.LLMCredential {
	return runcontroller.LLMCredential{
		Kind:  runcontroller.LLMCredentialAPIKey,
		Value: "test-key-not-logged",
	}
}

func testConfig() runcontroller.Config {
	return runcontroller.Config{
		IdleGrace:    30 * time.Second,
		ScanPeriod:   5 * time.Second,
		HelloTimeout: 60 * time.Second,
		Credential:   testCredential(),
	}
}

// harness assembles a Controller on a real PostgreSQL database, a fake
// provider, a fake clock and an in-process gRPC server on bufconn.
type harness struct {
	t        *testing.T
	ctrl     *runcontroller.Controller
	clock    *fakeclock.Clock
	provider *fakeprovider.Provider
	pool     *pgxpool.Pool

	srv    *grpc.Server
	lis    *bufconn.Listener
	cc     *grpc.ClientConn
	client agentprotocolv1.AgentStreamClient

	mu          sync.Mutex
	hook        func(op, runID string)
	afterCommit func(op, runID string)
	streams     []context.CancelFunc
}

func newHarness(t *testing.T, cfg runcontroller.Config) *harness {
	t.Helper()
	return newHarnessWithProvider(t, cfg, nil)
}

// newHarnessWithProvider builds a harness whose controller uses wrap(provider)
// instead of the bare fake provider. Tests use it to observe provider call
// return values.
func newHarnessWithProvider(t *testing.T, cfg runcontroller.Config, wrap func(sandboxprovider.Provider) sandboxprovider.Provider) *harness {
	t.Helper()

	pool := pgtest.NewPoolFromTemplate(t, "runcontroller", runcontroller.Migrate)
	provider := fakeprovider.New(t)
	clock := fakeclock.New(testEpoch)
	var controllerProvider sandboxprovider.Provider = provider
	if wrap != nil {
		controllerProvider = wrap(controllerProvider)
	}
	ctrl := runcontroller.New(cfg, pool, controllerProvider, clock)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ctrl.RegisterGRPC(srv)
	go func() { _ = srv.Serve(lis) }()

	cc, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("创建 gRPC 客户端: %v", err)
	}

	h := &harness{
		t:        t,
		ctrl:     ctrl,
		clock:    clock,
		provider: provider,
		pool:     pool,
		srv:      srv,
		lis:      lis,
		cc:       cc,
		client:   agentprotocolv1.NewAgentStreamClient(cc),
	}
	runcontroller.SetAfterLockHook(ctrl, h.callHook)
	runcontroller.SetAfterCommitHook(ctrl, h.callAfterCommit)

	t.Cleanup(func() {
		h.mu.Lock()
		for _, cancel := range h.streams {
			cancel()
		}
		h.streams = nil
		h.mu.Unlock()

		_ = cc.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return h
}

func (h *harness) callHook(op, runID string) {
	h.mu.Lock()
	fn := h.hook
	h.mu.Unlock()
	if fn != nil {
		fn(op, runID)
	}
}

func (h *harness) setHook(fn func(op, runID string)) {
	h.mu.Lock()
	h.hook = fn
	h.mu.Unlock()
}

func (h *harness) callAfterCommit(op, runID string) {
	h.mu.Lock()
	fn := h.afterCommit
	h.mu.Unlock()
	if fn != nil {
		fn(op, runID)
	}
}

func (h *harness) setAfterCommitHook(fn func(op, runID string)) {
	h.mu.Lock()
	h.afterCommit = fn
	h.mu.Unlock()
}

func (h *harness) openStream() agentprotocolv1.AgentStream_ConnectClient {
	h.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.Connect(ctx)
	if err != nil {
		cancel()
		h.t.Fatalf("打开流: %v", err)
	}
	h.mu.Lock()
	h.streams = append(h.streams, cancel)
	h.mu.Unlock()
	return stream
}

func send(t *testing.T, stream agentprotocolv1.AgentStream_ConnectClient, frame *agentprotocolv1.WardenFrame) {
	t.Helper()
	if err := stream.Send(frame); err != nil {
		t.Fatalf("发送帧: %v", err)
	}
}

func helloFrame(runID string, phase agentprotocolv1.Phase, turnID string, lastAccepted int64, lastTurn *agentprotocolv1.LastTurn) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Hello{
			Hello: &agentprotocolv1.Hello{
				AgentState:      &agentprotocolv1.AgentState{Phase: phase, TurnId: turnID},
				LastAcceptedSeq: lastAccepted,
				LastTurn:        lastTurn,
			},
		},
	}
}

func acceptedFrame(runID string, throughSeq int64) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Accepted{
			// The protocol requires a non-empty delivery_id; the control plane
			// only uses through_seq.
			Accepted: &agentprotocolv1.Accepted{DeliveryId: "delivery-1", ThroughSeq: throughSeq},
		},
	}
}

func turnStartedFrame(runID, turnID, deliveryID string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: turnID,
				Kind:   &agentprotocolv1.AgentEvent_TurnStarted{TurnStarted: &agentprotocolv1.TurnStarted{DeliveryId: deliveryID}},
			},
		},
	}
}

func turnEndedFrame(runID, turnID string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: turnID,
				Kind:   &agentprotocolv1.AgentEvent_TurnEnded{TurnEnded: &agentprotocolv1.TurnEnded{Output: "done"}},
			},
		},
	}
}

// stepToBoundary creates a run, opens a stream and walks it to
// active/boundary after the first inbox message has been delivered and its
// turn has ended.
func (h *harness) stepToBoundary() (string, agentprotocolv1.AgentStream_ConnectClient) {
	h.t.Helper()

	runID := h.createRun("first message")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream := h.openStream()
	send(h.t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

	send(h.t, stream, acceptedFrame(runID, 1))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	send(h.t, stream, turnStartedFrame(runID, "turn-1", "delivery-1"))
	send(h.t, stream, turnEndedFrame(runID, "turn-1"))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	return runID, stream
}

func (h *harness) createRun(prompt string) string {
	h.t.Helper()
	runID, err := h.ctrl.Create(context.Background(), prompt)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return runID
}

func (h *harness) waitCalls(op fakeprovider.Op, n int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.provider.WaitForCalls(ctx, op, n); err != nil {
		h.t.Fatalf("等待 %s 调用: %v", op, err)
	}
}

func (h *harness) callCount(op fakeprovider.Op) int {
	return len(h.provider.Calls(op))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("超时等待 %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) detail(runID string) (runcontroller.RunDetail, error) {
	return h.ctrl.Get(context.Background(), runID)
}

func (h *harness) waitState(runID string, state runcontroller.State, substate runcontroller.Substate) runcontroller.RunDetail {
	h.t.Helper()
	deadline := time.Now().Add(testTimeout)
	var (
		last    runcontroller.RunDetail
		lastErr error
	)
	for {
		d, err := h.detail(runID)
		if err != nil {
			lastErr = err
		} else {
			last = d
			lastErr = nil
		}
		if lastErr == nil && d.State == state && d.Substate == substate {
			return last
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("超时等待 %s/%s；实际 %s/%s，失败原因 %q，最近查询错误 %v",
				state, substate, last.State, last.Substate, last.FailureReason, lastErr)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) state(runID string) (runcontroller.State, runcontroller.Substate) {
	h.t.Helper()
	d, err := h.detail(runID)
	if err != nil {
		h.t.Fatalf("Get %s: %v", runID, err)
	}
	return d.State, d.Substate
}

func (h *harness) provisionReturned(runID string) bool {
	var at *time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT provision_returned_at FROM run WHERE run_id = $1`, runID).Scan(&at)
	if err != nil {
		h.t.Fatalf("查询 provision_returned_at: %v", err)
	}
	return at != nil
}

func (h *harness) restoreReturned(runID string) bool {
	var at *time.Time
	err := h.pool.QueryRow(context.Background(),
		`SELECT restore_returned_at FROM suspension WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
		runID).Scan(&at)
	if err != nil {
		h.t.Fatalf("查询 restore_returned_at: %v", err)
	}
	return at != nil
}

func (h *harness) waitProvisionReturned(runID string) {
	h.t.Helper()
	eventually(h.t, "provision_returned_at", func() bool { return h.provisionReturned(runID) })
}

func (h *harness) waitRestoreReturned(runID string) {
	h.t.Helper()
	eventually(h.t, "restore_returned_at", func() bool { return h.restoreReturned(runID) })
}

func (h *harness) latestSuspension(runID string) (snapshotMS, restoreMS *int64, errText string) {
	h.t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT snapshot_ms, restore_ms, error FROM suspension
		 WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`, runID).Scan(&snapshotMS, &restoreMS, &errText)
	if err != nil {
		h.t.Fatalf("查询 suspension: %v", err)
	}
	return snapshotMS, restoreMS, errText
}

// startRun runs the controller scan loop until the test ends.
func (h *harness) startRun() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ctrl.Run(ctx) }()

	if err := h.clock.WaitForTimers(context.Background(), 1); err != nil {
		cancel()
		h.t.Fatalf("等待扫描计时器: %v", err)
	}
	h.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("Run 返回错误: %v", err)
			}
		case <-time.After(testTimeout):
			h.t.Errorf("Run 没有在关停后返回")
		}
	})
}

// advanceScan advances the fake clock by d and waits until the scan round it
// triggers has finished (the loop created its next timer).
func (h *harness) advanceScan(d time.Duration) {
	h.t.Helper()
	if err := h.clock.WaitForTimers(context.Background(), 1); err != nil {
		h.t.Fatalf("等待扫描计时器: %v", err)
	}
	h.clock.Advance(d)
	if err := h.clock.WaitForTimers(context.Background(), 1); err != nil {
		h.t.Fatalf("等待下一轮扫描计时器: %v", err)
	}
}

// waitForLockWaiter confirms, from a connection outside the 2-connection test
// pool, that a transaction is waiting for a row lock.
func (h *harness) waitForLockWaiter() {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, h.pool.Config().ConnConfig.ConnString())
	if err != nil {
		h.t.Fatalf("打开独立连接: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	deadline := time.Now().Add(testTimeout)
	for {
		var waiting int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query ILIKE '%FOR UPDATE%'`).Scan(&waiting)
		if err != nil {
			h.t.Fatalf("查询 pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("没有观察到等待行锁的事务")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) expectShutdownAndClose(stream agentprotocolv1.AgentStream_ConnectClient) {
	h.t.Helper()

	type result struct {
		shutdown bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		sawShutdown := false
		for {
			frame, err := stream.Recv()
			if err != nil {
				done <- result{shutdown: sawShutdown, err: err}
				return
			}
			if frame.GetShutdown() != nil {
				sawShutdown = true
			}
		}
	}()

	select {
	case r := <-done:
		if !r.shutdown {
			h.t.Errorf("关闭前没有收到 Shutdown 帧")
		}
		if code := status.Code(r.err); code != codes.FailedPrecondition {
			h.t.Errorf("流关闭码 = %v, 想要 FailedPrecondition", code)
		}
	case <-time.After(testTimeout):
		h.t.Fatal("等待流关闭超时")
	}
}

func TestRunServiceErrorConventions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	ctx := context.Background()

	if _, err := h.ctrl.Get(ctx, "missing"); !errors.Is(err, runcontroller.ErrNotFound) {
		t.Fatalf("Get 不存在的运行实例: %v, 想要 ErrNotFound", err)
	}
	if _, err := h.ctrl.AppendInbox(ctx, "missing", "text"); !errors.Is(err, runcontroller.ErrNotFound) {
		t.Fatalf("AppendInbox 不存在的运行实例: %v, 想要 ErrNotFound", err)
	}
	if err := h.ctrl.End(ctx, "missing"); !errors.Is(err, runcontroller.ErrNotFound) {
		t.Fatalf("End 不存在的运行实例: %v, 想要 ErrNotFound", err)
	}

	if _, err := h.ctrl.Create(ctx, ""); !errors.Is(err, runcontroller.ErrInvalidText) {
		t.Fatalf("Create 空文本: %v, 想要 ErrInvalidText", err)
	}
	if _, err := h.ctrl.Create(ctx, strings.Repeat("a", runcontroller.MaxTextBytes+1)); !errors.Is(err, runcontroller.ErrInvalidText) {
		t.Fatalf("Create 超长文本: %v, 想要 ErrInvalidText", err)
	}

	runID := h.createRun("first")
	if _, err := h.ctrl.AppendInbox(ctx, runID, ""); !errors.Is(err, runcontroller.ErrInvalidText) {
		t.Fatalf("AppendInbox 空文本: %v, 想要 ErrInvalidText", err)
	}
	if _, err := h.ctrl.AppendInbox(ctx, runID, strings.Repeat("b", runcontroller.MaxTextBytes+1)); !errors.Is(err, runcontroller.ErrInvalidText) {
		t.Fatalf("AppendInbox 超长文本: %v, 想要 ErrInvalidText", err)
	}
	if seq, err := h.ctrl.AppendInbox(ctx, runID, strings.Repeat("c", runcontroller.MaxTextBytes)); err != nil || seq != 2 {
		t.Fatalf("AppendInbox 恰好 MaxTextBytes: seq=%d err=%v", seq, err)
	}

	if err := h.ctrl.End(ctx, runID); err != nil {
		t.Fatalf("End: %v", err)
	}
	h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
	if _, err := h.ctrl.AppendInbox(ctx, runID, "after end"); !errors.Is(err, runcontroller.ErrTerminated) {
		t.Fatalf("AppendInbox 已结束: %v, 想要 ErrTerminated", err)
	}
	if err := h.ctrl.End(ctx, runID); err != nil {
		t.Fatalf("End 幂等: %v", err)
	}

	h.provider.FailNext(fakeprovider.OpProvision, errors.New("provision blew up"))
	failedID := h.createRun("will fail")
	h.waitState(failedID, runcontroller.StateFailed, runcontroller.SubstateNone)
	if _, err := h.ctrl.AppendInbox(ctx, failedID, "after fail"); !errors.Is(err, runcontroller.ErrTerminated) {
		t.Fatalf("AppendInbox 已失败: %v, 想要 ErrTerminated", err)
	}
}

func TestRunServiceCreateProvisionFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	h.provider.FailNext(fakeprovider.OpProvision, errors.New("provision refused"))

	runID, err := h.ctrl.Create(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if runID == "" {
		t.Fatal("Create 返回了空 run_id")
	}

	detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
	if !strings.Contains(detail.FailureReason, "provision refused") {
		t.Fatalf("FailureReason = %q, 想要包含 provision refused", detail.FailureReason)
	}
}

func TestRunServiceListOrder(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	ctx := context.Background()

	first := h.createRun("one")
	h.clock.Advance(time.Second)
	second := h.createRun("two")
	h.clock.Advance(time.Second)
	third := h.createRun("three")

	runs, err := h.ctrl.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(runs))
	for i, run := range runs {
		got[i] = run.RunID
	}
	want := []string{third, second, first}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("List 顺序 = %v, 想要 %v（CreatedAt 降序）", got, want)
	}

	// Same CreatedAt is broken by run_id descending.
	h.clock.Advance(time.Second)
	alpha := h.createRun("alpha")
	beta := h.createRun("beta")
	runs, err = h.ctrl.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) < 2 {
		t.Fatalf("List 只有 %d 个运行实例", len(runs))
	}
	head := []string{runs[0].RunID, runs[1].RunID}
	wantHead := []string{beta, alpha}
	if beta < alpha {
		wantHead = []string{alpha, beta}
	}
	if strings.Join(head, ",") != strings.Join(wantHead, ",") {
		t.Fatalf("同一 CreatedAt 的顺序 = %v, 想要 %v", head, wantHead)
	}
}

func TestRunServiceEndStates(t *testing.T) {
	t.Parallel()

	t.Run("provisioning", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		release := h.provider.Hold(fakeprovider.OpProvision)
		defer release()

		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)

		release()
		h.waitCalls(fakeprovider.OpDestroy, 1)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("Destroy 调用 %d 次, 想要 1 次", got)
		}
	})

	t.Run("active", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, stream := h.stepToBoundary()

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
		h.expectShutdownAndClose(stream)
		h.waitCalls(fakeprovider.OpDestroy, 1)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End 幂等: %v", err)
		}
		// 队列屏障把计数放在任何可能被重复排入的 Destroy 之后。
		runcontroller.WaitProviderQueue(h.ctrl, runID)
		if got := h.callCount(fakeprovider.OpDestroy); got != 1 {
			t.Fatalf("ended 上重复 End 又调用了 Destroy，共 %d 次", got)
		}
	})

	t.Run("suspended", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()

		h.clock.Advance(testConfig().IdleGrace + time.Second)
		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
		h.waitCalls(fakeprovider.OpDestroy, 1)
	})

	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		h.provider.FailNext(fakeprovider.OpProvision, errors.New("provision refused"))
		runID := h.createRun("first")
		h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)

		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End failed: %v", err)
		}
		h.waitCalls(fakeprovider.OpDestroy, 1)
		state, substate := h.state(runID)
		if state != runcontroller.StateFailed || substate != runcontroller.SubstateNone {
			t.Fatalf("End failed 后状态 = %s/%s, 想要 failed", state, substate)
		}

		// Every End on a failed run queues Destroy again.
		if err := h.ctrl.End(context.Background(), runID); err != nil {
			t.Fatalf("End failed 再次: %v", err)
		}
		h.waitCalls(fakeprovider.OpDestroy, 2)
	})
}

func TestRunServiceCallerContext(t *testing.T) {
	t.Parallel()

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		release := h.provider.Hold(fakeprovider.OpProvision)
		t.Cleanup(release)

		ctx, cancel := context.WithCancel(context.Background())
		runID, err := h.ctrl.Create(ctx, "first")
		cancel()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		h.waitCalls(fakeprovider.OpProvision, 1)
		release()
		h.waitProvisionReturned(runID)

		state, _ := h.state(runID)
		if state != runcontroller.StateProvisioning {
			t.Fatalf("状态 = %s, 想要 provisioning", state)
		}
	})

	t.Run("append", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)
		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

		release := h.provider.Hold(fakeprovider.OpRestore)
		t.Cleanup(release)

		ctx, cancel := context.WithCancel(context.Background())
		seq, err := h.ctrl.AppendInbox(ctx, runID, "wake up")
		cancel()
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}
		h.waitCalls(fakeprovider.OpRestore, 1)
		release()
		h.waitRestoreReturned(runID)
		h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
	})

	t.Run("end", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()

		release := h.provider.Hold(fakeprovider.OpDestroy)
		t.Cleanup(release)

		ctx, cancel := context.WithCancel(context.Background())
		err := h.ctrl.End(ctx, runID)
		cancel()
		if err != nil {
			t.Fatalf("End: %v", err)
		}
		h.waitCalls(fakeprovider.OpDestroy, 1)
		release()
		h.waitState(runID, runcontroller.StateEnded, runcontroller.SubstateNone)
	})
}

func TestControllerProvisioningHelloTimeout(t *testing.T) {
	t.Parallel()

	t.Run("expires", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig()
		h := newHarness(t, cfg)
		h.startRun()

		runID := h.createRun("first")
		h.waitProvisionReturned(runID)

		h.advanceScan(cfg.HelloTimeout + cfg.ScanPeriod)
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "hello timeout") {
			t.Fatalf("FailureReason = %q, 想要 hello timeout", detail.FailureReason)
		}
	})

	t.Run("provision still held", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig()
		h := newHarness(t, cfg)
		h.startRun()

		release := h.provider.Hold(fakeprovider.OpProvision)
		t.Cleanup(release)
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)

		h.advanceScan(cfg.HelloTimeout + cfg.ScanPeriod)
		h.advanceScan(cfg.ScanPeriod)

		state, _ := h.state(runID)
		if state != runcontroller.StateProvisioning {
			t.Fatalf("Provision 未返回时状态 = %s, 想要 provisioning", state)
		}
		if h.provisionReturned(runID) {
			t.Fatal("Provision 未返回却写入了 provision_returned_at")
		}
	})
}

// provisionReturn records how a provider call returned.
type provisionReturn struct {
	runID  string
	ctxErr error
}

// provisionReturnGate wraps a provider and blocks Provision after it has
// returned until release is called. It lets the test observe both that the
// call returned with context.Canceled and that Run waited for it.
type provisionReturnGate struct {
	sandboxprovider.Provider
	returned chan provisionReturn
	release  chan struct{}
	once     sync.Once
}

func (p *provisionReturnGate) Provision(ctx context.Context, runID string) (sandboxprovider.Timing, error) {
	timing, err := p.Provider.Provision(ctx, runID)
	p.returned <- provisionReturn{runID: runID, ctxErr: ctx.Err()}
	<-p.release
	return timing, err
}

func (p *provisionReturnGate) open() {
	p.once.Do(func() { close(p.release) })
}

func TestControllerRunCancelWaitsForProviderCalls(t *testing.T) {
	t.Parallel()

	gate := &provisionReturnGate{
		returned: make(chan provisionReturn, 1),
		release:  make(chan struct{}),
	}
	h := newHarnessWithProvider(t, testConfig(), func(p sandboxprovider.Provider) sandboxprovider.Provider {
		gate.Provider = p
		return gate
	})
	release := h.provider.Hold(fakeprovider.OpProvision)
	t.Cleanup(func() {
		gate.open()
		release()
	})

	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ctrl.Run(ctx) }()
	if err := h.clock.WaitForTimers(context.Background(), 1); err != nil {
		t.Fatalf("等待扫描计时器: %v", err)
	}

	cancel()

	// Run must call shutdown: the held Provision only returns when the queue's
	// context is canceled, and it must see context.Canceled.
	var ret provisionReturn
	select {
	case ret = <-gate.returned:
	case <-time.After(testTimeout):
		t.Fatal("取消 Run 后 Provision 没有返回：Run 没有调 shutdown")
	}
	if !errors.Is(ret.ctxErr, context.Canceled) {
		t.Fatalf("Provision 返回时的 ctx 错误 = %v, 想要 context.Canceled", ret.ctxErr)
	}
	if ret.runID != runID {
		t.Fatalf("Provision 返回的 run_id = %q, 想要 %q", ret.runID, runID)
	}

	// Provision has not returned from the provider queue's point of view yet,
	// so Run must still be waiting.
	select {
	case err := <-done:
		t.Fatalf("Provision 尚未返回，Run 却已返回: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	gate.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回 %v, 想要 nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run 没有在提供者调用取消后返回")
	}

	state, _ := h.state(runID)
	if state != runcontroller.StateProvisioning {
		t.Fatalf("状态 = %s, 想要停在 provisioning", state)
	}
	if h.provisionReturned(runID) {
		t.Fatal("被取消的 Provision 不应写完成事件")
	}
}

func TestControllerSuspendDisabled(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.SuspendEnabled = false
	h := newHarness(t, cfg)
	h.startRun()

	runID, _ := h.stepToBoundary()
	h.advanceScan(cfg.IdleGrace + cfg.ScanPeriod)

	state, substate := h.state(runID)
	if state != runcontroller.StateActive || substate != runcontroller.SubstateBoundary {
		t.Fatalf("SuspendEnabled=false 时状态 = %s/%s, 想要 active/boundary", state, substate)
	}
	if got := h.callCount(fakeprovider.OpSnapshot); got != 0 {
		t.Fatalf("SuspendEnabled=false 却调用了 %d 次 Snapshot", got)
	}

	// T_hello is still enforced while suspension is disabled.
	provisioning := h.createRun("first")
	h.waitProvisionReturned(provisioning)
	h.advanceScan(cfg.HelloTimeout + cfg.ScanPeriod)
	h.waitState(provisioning, runcontroller.StateFailed, runcontroller.SubstateNone)
}

func TestControllerSnapshotFailure(t *testing.T) {
	t.Parallel()

	t.Run("not crashed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)

		h.provider.FailNext(fakeprovider.OpSnapshot, errors.New("snapshot failed"))
		h.provider.FailNext(fakeprovider.OpGetSandbox, errors.New("get sandbox failed"))

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitCalls(fakeprovider.OpGetSandbox, 1)

		if _, _, errText := h.latestSuspension(runID); !strings.Contains(errText, "snapshot failed") {
			t.Fatalf("suspension.error = %q, 想要包含 snapshot failed", errText)
		}
	})

	t.Run("crashed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _ := h.stepToBoundary()
		h.clock.Advance(testConfig().IdleGrace + time.Second)
		h.provider.SetCrashed(sandboxprovider.ActorName(runID))

		if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "crashed") {
			t.Fatalf("FailureReason = %q, 想要包含 crashed", detail.FailureReason)
		}
	})
}
