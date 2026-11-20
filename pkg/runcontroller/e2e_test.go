package runcontroller_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
	"github.com/wangzw/castworks/internal/testutil/fakewarden"
	"github.com/wangzw/castworks/internal/testutil/pgtest"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// e2eHarness assembles a Controller on a real PostgreSQL database, a fake
// provider, a fake clock and an in-process gRPC server on bufconn, and wires
// a fake warden process to every run the provider provisions. The provider
// callbacks model the lifecycle of a real actor: a successful Provision
// starts the warden, Snapshot freezes it and RestoreSnapshot thaws it.
type e2eHarness struct {
	t        *testing.T
	cfg      runcontroller.Config
	pool     *pgxpool.Pool
	provider *fakeprovider.Provider
	clock    *fakeclock.Clock

	mu         sync.Mutex
	ctrl       *runcontroller.Controller
	srv        *grpc.Server
	lis        *bufconn.Listener
	wardens    map[string]*fakewarden.Warden
	wardenInit func(*fakewarden.Warden)
	runCancel  context.CancelFunc
	runDone    chan error
	hook       func(op, runID string)
}

// e2eConfig is testConfig with the suspension scan enabled: the e2e tests
// drive suspension through the scan loop and the fake clock.
func e2eConfig() runcontroller.Config {
	cfg := testConfig()
	cfg.SuspendEnabled = true
	return cfg
}

func newE2E(t *testing.T, cfg runcontroller.Config) *e2eHarness {
	t.Helper()

	pool := pgtest.NewPoolFromTemplate(t, "runcontroller", runcontroller.Migrate)

	h := &e2eHarness{
		t:        t,
		cfg:      cfg,
		pool:     pool,
		provider: fakeprovider.New(t),
		clock:    fakeclock.New(testEpoch),
		wardens:  make(map[string]*fakewarden.Warden),
	}
	h.provider.OnProvisioned(h.onProvisioned)
	h.provider.OnSnapshotted(func(actor string) {
		if w := h.wardenByActor(actor); w != nil {
			w.Freeze()
		}
	})
	h.provider.OnRestored(func(actor string) {
		if w := h.wardenByActor(actor); w != nil {
			w.Thaw()
		}
	})
	h.startControlPlane()
	t.Cleanup(h.close)
	return h
}

func (h *e2eHarness) startControlPlane() {
	ctrl := runcontroller.New(h.cfg, h.pool, h.provider, h.clock)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ctrl.RegisterGRPC(srv)
	go func() { _ = srv.Serve(lis) }()

	h.mu.Lock()
	h.ctrl = ctrl
	h.lis = lis
	h.srv = srv
	hook := h.hook
	h.mu.Unlock()

	runcontroller.SetAfterLockHook(ctrl, hook)
}

func (h *e2eHarness) close() {
	h.mu.Lock()
	ws := make([]*fakewarden.Warden, 0, len(h.wardens))
	for _, w := range h.wardens {
		ws = append(ws, w)
	}
	h.mu.Unlock()

	for _, w := range ws {
		w.Stop()
	}
	h.stopRun()

	h.mu.Lock()
	srv, lis := h.srv, h.lis
	h.srv, h.lis = nil, nil
	h.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
	if lis != nil {
		_ = lis.Close()
	}
}

// dial opens a new connection to the control plane's current listener. The
// warden owns the returned connection and closes it when the stream ends.
func (h *e2eHarness) dial(context.Context) (*grpc.ClientConn, error) {
	h.mu.Lock()
	lis := h.lis
	h.mu.Unlock()
	if lis == nil {
		return nil, errors.New("e2e: control plane is not running")
	}
	return grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

func (h *e2eHarness) onProvisioned(runID string) {
	w := fakewarden.New(h.t, runID, h.dial)

	h.mu.Lock()
	h.wardens[runID] = w
	init := h.wardenInit
	h.mu.Unlock()

	if init != nil {
		init(w)
	}
	w.Start()
}

func (h *e2eHarness) setWardenInit(fn func(*fakewarden.Warden)) {
	h.mu.Lock()
	h.wardenInit = fn
	h.mu.Unlock()
}

func (h *e2eHarness) wardenByActor(actor string) *fakewarden.Warden {
	runID := strings.TrimPrefix(actor, "cw-")
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wardens[runID]
}

func (h *e2eHarness) warden(runID string) *fakewarden.Warden {
	h.t.Helper()
	var w *fakewarden.Warden
	eventually(h.t, "运行实例的假看守进程", func() bool {
		h.mu.Lock()
		w = h.wardens[runID]
		h.mu.Unlock()
		return w != nil
	})
	return w
}

func (h *e2eHarness) controller() *runcontroller.Controller {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctrl
}

func (h *e2eHarness) createRun(prompt string) string {
	h.t.Helper()
	runID, err := h.controller().Create(context.Background(), prompt)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return runID
}

func (h *e2eHarness) appendInbox(runID, text string) (int64, error) {
	return appendHuman(h.controller(), context.Background(), runID, text)
}

func (h *e2eHarness) detail(runID string) (runcontroller.RunDetail, error) {
	return h.controller().Get(context.Background(), runID)
}

func (h *e2eHarness) state(runID string) (runcontroller.State, runcontroller.Substate) {
	h.t.Helper()
	d, err := h.detail(runID)
	if err != nil {
		h.t.Fatalf("Get %s: %v", runID, err)
	}
	return d.State, d.Substate
}

func (h *e2eHarness) waitState(runID string, state runcontroller.State, substate runcontroller.Substate) runcontroller.RunDetail {
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

func (h *e2eHarness) waitCalls(op fakeprovider.Op, n int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.provider.WaitForCalls(ctx, op, n); err != nil {
		h.t.Fatalf("等待 %s 调用: %v", op, err)
	}
}

func (h *e2eHarness) callCount(op fakeprovider.Op) int {
	return len(h.provider.Calls(op))
}

func (h *e2eHarness) waitWritten(w *fakewarden.Warden, n int) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("warden 写入 %d 条消息", n), func() bool {
		return len(w.Written()) >= n
	})
}

func (h *e2eHarness) waitRestoreReturned(runID string) {
	h.t.Helper()
	eventually(h.t, "restore_returned_at", func() bool {
		var at *time.Time
		err := h.pool.QueryRow(context.Background(),
			`SELECT restore_returned_at FROM suspension WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
			runID).Scan(&at)
		if err != nil {
			h.t.Fatalf("查询 restore_returned_at: %v", err)
		}
		return at != nil
	})
}

// turnDeliveryID reads the delivery_id stored for a turn. An empty string means
// the agent started the turn on its own.
func (h *e2eHarness) turnDeliveryID(runID, turnID string) string {
	h.t.Helper()
	var deliveryID string
	err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE(delivery_id, '') FROM turn WHERE run_id = $1 AND turn_id = $2`,
		runID, turnID).Scan(&deliveryID)
	if err != nil {
		h.t.Fatalf("查询回合 %s: %v", turnID, err)
	}
	return deliveryID
}

// startRun runs the controller scan loop until stopRun or close.
func (h *e2eHarness) startRun() {
	h.t.Helper()

	ctrl := h.controller()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ctrl.Run(ctx) }()

	timerCtx, timerCancel := context.WithTimeout(context.Background(), testTimeout)
	defer timerCancel()
	if err := h.clock.WaitForTimers(timerCtx, 1); err != nil {
		cancel()
		h.t.Fatalf("等待扫描计时器: %v", err)
	}

	h.mu.Lock()
	h.runCancel = cancel
	h.runDone = done
	h.mu.Unlock()
}

func (h *e2eHarness) stopRun() {
	h.mu.Lock()
	cancel, done := h.runCancel, h.runDone
	h.runCancel, h.runDone = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			h.t.Errorf("Run 返回错误: %v", err)
		}
	case <-time.After(testTimeout):
		h.t.Errorf("Run 没有在关停后返回")
	}
}

// restartControlPlane stops the scan loop and the gRPC server, builds a new
// Controller over the same database, provider and clock, and points every
// fake warden at the new listener.
func (h *e2eHarness) restartControlPlane() {
	h.t.Helper()

	h.stopRun()

	h.mu.Lock()
	srv, lis := h.srv, h.lis
	h.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
	if lis != nil {
		_ = lis.Close()
	}

	h.startControlPlane()
	h.startRun()

	h.mu.Lock()
	ws := make([]*fakewarden.Warden, 0, len(h.wardens))
	for _, w := range h.wardens {
		ws = append(ws, w)
	}
	h.mu.Unlock()
	for _, w := range ws {
		w.SetDial(h.dial)
	}
}

// advanceScan moves the fake clock by d, fires the scan timer and waits until
// the scan round has finished and the loop created its next timer.
func (h *e2eHarness) advanceScan(d time.Duration) {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.clock.WaitForTimers(ctx, 1); err != nil {
		h.t.Fatalf("等待扫描计时器: %v", err)
	}
	h.clock.Advance(d)
	if err := h.clock.WaitForTimers(ctx, 1); err != nil {
		h.t.Fatalf("等待下一轮扫描计时器: %v", err)
	}
}

func (h *e2eHarness) setHook(fn func(op, runID string)) {
	h.mu.Lock()
	h.hook = fn
	ctrl := h.ctrl
	h.mu.Unlock()
	runcontroller.SetAfterLockHook(ctrl, fn)
}

// suspendNow advances the clock past G_idle and runs the suspension rule
// immediately.
func (h *e2eHarness) suspendNow(runID string) {
	h.t.Helper()
	h.clock.Advance(h.cfg.IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.controller(), runID); err != nil {
		h.t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)
}

func assertWritten(t *testing.T, got []int64, want int) {
	t.Helper()
	if len(got) != want {
		t.Fatalf("warden 写入 %v, 想要恰好 %d 条消息", got, want)
	}
	for i, seq := range got {
		if seq != int64(i+1) {
			t.Fatalf("warden 写入序列 = %v, 想要 1..%d 且不重复", got, want)
		}
	}
}

// assertDeliverCounts requires every seq in 1..want to have reached the warden
// exactly once. The fake deduplicates what it writes, so a duplicate Deliver
// would otherwise be invisible.
func assertDeliverCounts(t *testing.T, w *fakewarden.Warden, want int) {
	t.Helper()
	counts := w.DeliverCounts()
	for seq := 1; seq <= want; seq++ {
		if got := counts[int64(seq)]; got != 1 {
			t.Fatalf("消息 %d 的 Deliver 次数 = %d, 想要恰好 1 次", seq, got)
		}
	}
	if len(counts) != want {
		t.Fatalf("Deliver 覆盖 %d 个序号, 想要 %d", len(counts), want)
	}
}

// assertTurnCount requires the database to hold exactly want turns for runID,
// each of them ended.
func assertTurnCount(t *testing.T, h *e2eHarness, runID string, want int) {
	t.Helper()
	detail, err := h.detail(runID)
	if err != nil {
		t.Fatalf("Get %s: %v", runID, err)
	}
	if len(detail.Turns) != want {
		t.Fatalf("库内回合数 = %d, 想要恰好 %d 条", len(detail.Turns), want)
	}
	for _, turn := range detail.Turns {
		if turn.EndedAt == nil {
			t.Fatalf("回合 %s 没有结束时间", turn.TurnID)
		}
	}
}

// assertNoDeliverOnCurrentStream requires seq not to appear in a Deliver on the
// warden's current stream. A duplicate is sent right after the HelloAck that
// triggered it, so a short observation window is enough.
func assertNoDeliverOnCurrentStream(t *testing.T, w *fakewarden.Warden, seq int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := w.WaitFor(ctx, func() bool { return w.CurrentStreamDeliverCounts()[seq] > 0 }); err == nil {
		t.Fatalf("新流上消息 %d 被重复投递", seq)
	}
}

// ---------------------------------------------------------------------------
// 共享的原始流工具：用于强制两种事务先后顺序。

// appendResult carries one AppendInbox outcome out of a goroutine.
type appendResult struct {
	seq int64
	err error
}

// openStreamWithCancel opens a stream on h's server and returns its cancel
// function so a test can close it at a chosen moment.
func openStreamWithCancel(t *testing.T, h *harness) (agentprotocolv1.AgentStream_ConnectClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := h.client.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("打开流: %v", err)
	}
	return stream, cancel
}

// controlRecorder drains a stream in the background and records every frame
// the control plane sends.
type controlRecorder struct {
	mu     sync.Mutex
	frames []*agentprotocolv1.ControlFrame
	err    error
}

func recordControl(stream agentprotocolv1.AgentStream_ConnectClient) *controlRecorder {
	r := &controlRecorder{}
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			r.frames = append(r.frames, frame)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *controlRecorder) snapshot() []*agentprotocolv1.ControlFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*agentprotocolv1.ControlFrame(nil), r.frames...)
}

func (r *controlRecorder) helloAckSeen() bool {
	for _, f := range r.snapshot() {
		if f.GetHelloAck() != nil {
			return true
		}
	}
	return false
}

func (r *controlRecorder) deliverSeqs() []int64 {
	var seqs []int64
	for _, f := range r.snapshot() {
		d := f.GetDeliver()
		if d == nil {
			continue
		}
		for _, m := range d.GetBatch() {
			seqs = append(seqs, m.GetInboxSeq())
		}
	}
	return seqs
}

func (r *controlRecorder) countDeliverSeq(seq int64) int {
	n := 0
	for _, got := range r.deliverSeqs() {
		if got == seq {
			n++
		}
	}
	return n
}

func (r *controlRecorder) shutdownSeen() bool {
	for _, f := range r.snapshot() {
		if f.GetShutdown() != nil {
			return true
		}
	}
	return false
}

func (r *controlRecorder) firstFrameIsHelloAck() bool {
	frames := r.snapshot()
	return len(frames) > 0 && frames[0].GetHelloAck() != nil
}

// stepToBoundaryCancelable walks a run to active/boundary on a stream the test
// can cancel.
func stepToBoundaryCancelable(t *testing.T, h *harness) (string, agentprotocolv1.AgentStream_ConnectClient, context.CancelFunc) {
	t.Helper()

	runID := h.createRun("first message")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream, cancel := openStreamWithCancel(t, h)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

	send(t, stream, acceptedFrame(runID, 1))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	send(t, stream, turnStartedFrame(runID, "turn-1", "delivery-1"))
	send(t, stream, turnEndedFrame(runID, "turn-1"))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	return runID, stream, cancel
}

// awaitLockWaiter waits until some transaction is blocked on a row lock.
func awaitLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatalf("打开独立连接: %v", err)
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
			t.Fatalf("查询 pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有观察到等待行锁的事务")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// rowLockBarrier takes the run's row lock without advancing the fake clock.
// Competing transactions queue behind it, so when it returns every earlier
// transaction has committed and a single state read is deterministic.
func rowLockBarrier(t *testing.T, ctrl *runcontroller.Controller, runID string) {
	t.Helper()
	if err := runcontroller.TrySuspendNow(ctrl, runID); err != nil {
		t.Fatalf("取行锁屏障: %v", err)
	}
}

// awaitNoLockWaiter waits until no transaction is blocked on a row lock.
func awaitNoLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatalf("打开独立连接: %v", err)
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
			t.Fatalf("查询 pg_stat_activity: %v", err)
		}
		if waiting == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待行锁的事务没有结束")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// runcontroller.md“测试”一节五条。

// TestE2ESnapshotDuringAppend covers “Snapshot 进行中写入消息：快照完成后
// 立即恢复，消息被投递一次” and both orders of the Snapshot/turn-boundary
// interleaving: the message commits before the snapshot completion, or the
// snapshot completion commits first and the message wakes the suspended run.
func TestE2ESnapshotDuringAppend(t *testing.T) {
	t.Parallel()

	t.Run("append before snapshot completes", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		release := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(release)

		h.clock.Advance(testConfig().IdleGrace + time.Second)
		if err := runcontroller.TrySuspendNow(h.controller(), runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitCalls(fakeprovider.OpSnapshot, 1)

		seq, err := h.appendInbox(runID, "during snapshot")
		if err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		if seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", seq)
		}

		state, _ := h.state(runID)
		if state != runcontroller.StateSuspending {
			t.Fatalf("快照进行中状态 = %s, 想要 suspending", state)
		}

		release()
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 2)

		assertWritten(t, w.Written(), 2)
		assertDeliverCounts(t, w, 2)
		assertTurnCount(t, h, runID, 2)
		if got := h.callCount(fakeprovider.OpRestore); got != 1 {
			t.Fatalf("RestoreSnapshot 调用 %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("snapshot completes first", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		release := h.provider.Hold(fakeprovider.OpSnapshot)
		t.Cleanup(release)

		h.clock.Advance(testConfig().IdleGrace + time.Second)
		if err := runcontroller.TrySuspendNow(h.controller(), runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		h.waitCalls(fakeprovider.OpSnapshot, 1)

		gate := newLockGate("snapshot_done")
		h.setHook(gate.hook)
		t.Cleanup(func() {
			gate.open()
			h.setHook(nil)
		})

		release()
		gate.waitEntered(t)

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := h.appendInbox(runID, "after snapshot")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		awaitLockWaiter(t, h.pool)

		gate.open()
		res := <-appendDone
		if res.err != nil {
			t.Fatalf("AppendInbox: %v", res.err)
		}
		if res.seq != 2 {
			t.Fatalf("inbox_seq = %d, 想要 2", res.seq)
		}

		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 2)
		assertWritten(t, w.Written(), 2)
		assertDeliverCounts(t, w, 2)
		assertTurnCount(t, h, runID, 2)
		if got := h.callCount(fakeprovider.OpRestore); got != 1 {
			t.Fatalf("RestoreSnapshot 调用 %d 次, 想要恰好 1 次", got)
		}
	})
}

// TestE2ESuspendRestoreTenTimes covers “同一运行实例连续挂起、恢复 10 次，
// 每次恢复后发一条消息”.
func TestE2ESuspendRestoreTenTimes(t *testing.T) {
	t.Parallel()

	const cycles = 10
	cfg := e2eConfig()
	h := newE2E(t, cfg)
	h.startRun()

	runID := h.createRun("message 1")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	for i := 2; i <= cycles+1; i++ {
		h.advanceScan(cfg.IdleGrace + cfg.ScanPeriod)
		h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

		seq, err := h.appendInbox(runID, fmt.Sprintf("message %d", i))
		if err != nil {
			t.Fatalf("第 %d 轮 AppendInbox: %v", i-1, err)
		}
		if seq != int64(i) {
			t.Fatalf("第 %d 轮 inbox_seq = %d, 想要 %d", i-1, seq, i)
		}

		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, i)
		if got := h.callCount(fakeprovider.OpSnapshot); got != i-1 {
			t.Fatalf("第 %d 轮后 Snapshot 调用 %d 次, 想要 %d 次", i-1, got, i-1)
		}
		if got := h.callCount(fakeprovider.OpRestore); got != i-1 {
			t.Fatalf("第 %d 轮后 RestoreSnapshot 调用 %d 次, 想要 %d 次", i-1, got, i-1)
		}
	}

	detail, err := h.detail(runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertWritten(t, w.Written(), cycles+1)
	assertDeliverCounts(t, w, cycles+1)
	if len(detail.Turns) != cycles+1 {
		t.Fatalf("回合数 = %d, 想要 %d（与收件箱条数相等）", len(detail.Turns), cycles+1)
	}
	for _, turn := range detail.Turns {
		if turn.EndedAt == nil {
			t.Fatalf("回合 %s 没有结束时间", turn.TurnID)
		}
	}
	if len(detail.Suspensions) != cycles {
		t.Fatalf("挂起记录 = %d 条, 想要 %d 条", len(detail.Suspensions), cycles)
	}
	for i, susp := range detail.Suspensions {
		if susp.SnapshotMS == nil || susp.RestoreMS == nil || susp.HelloMS == nil {
			t.Fatalf("第 %d 条挂起记录缺少耗时: snapshot=%v restore=%v hello=%v",
				i+1, susp.SnapshotMS, susp.RestoreMS, susp.HelloMS)
		}
	}
	if got := h.callCount(fakeprovider.OpSnapshot); got != cycles {
		t.Fatalf("Snapshot 调用 %d 次, 想要 %d 次", got, cycles)
	}
	if got := h.callCount(fakeprovider.OpRestore); got != cycles {
		t.Fatalf("RestoreSnapshot 调用 %d 次, 想要 %d 次", got, cycles)
	}
}

// TestE2ERestartWhileSuspended covers “运行实例处于 suspended 时重启控制面，
// 之后写入的消息使它恢复并被投递”.
func TestE2ERestartWhileSuspended(t *testing.T) {
	t.Parallel()

	h := newE2E(t, e2eConfig())
	h.startRun()

	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	h.suspendNow(runID)
	h.restartControlPlane()

	seq, err := h.appendInbox(runID, "after restart")
	if err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	if seq != 2 {
		t.Fatalf("inbox_seq = %d, 想要 2", seq)
	}

	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 2)
	assertWritten(t, w.Written(), 2)
	assertDeliverCounts(t, w, 2)
	if got := h.callCount(fakeprovider.OpRestore); got != 1 {
		t.Fatalf("RestoreSnapshot 调用 %d 次, 想要 1 次", got)
	}
}

// TestE2ESubscriptionCompletionWakesSuspended covers the v0.2.0 test
// “订阅完成事件唤醒已挂起的运行实例” through the fake warden.
func TestE2ESubscriptionCompletionWakesSuspended(t *testing.T) {
	t.Parallel()

	h := newE2E(t, e2eConfig())
	h.startRun()

	runID := h.createRun("first")
	w := h.warden(runID)
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 1)

	h.suspendNow(runID)

	seq, duplicate, err := h.controller().AppendInbox(
		context.Background(), runID, testSubscriptionEvent("e2e-sub-1"))
	if err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	if duplicate || seq != 2 {
		t.Fatalf("AppendInbox = (%d, %v), 想要 (2, false)", seq, duplicate)
	}

	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
	h.waitWritten(w, 2)
	assertWritten(t, w.Written(), 2)
	assertDeliverCounts(t, w, 2)
}

// TestE2EFailureTransitions covers “Snapshot 失败、RestoreSnapshot 失败与等待
// Hello 超时三种情形的状态转移符合上文”.
func TestE2EFailureTransitions(t *testing.T) {
	t.Parallel()

	t.Run("snapshot error keeps boundary", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		h.clock.Advance(testConfig().IdleGrace + time.Second)
		h.provider.FailNext(fakeprovider.OpSnapshot, errors.New("snapshot exploded"))
		h.provider.FailNext(fakeprovider.OpGetSandbox, errors.New("get sandbox exploded"))
		if err := runcontroller.TrySuspendNow(h.controller(), runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}

		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitCalls(fakeprovider.OpGetSandbox, 1)
		var errText string
		if err := h.pool.QueryRow(context.Background(),
			`SELECT error FROM suspension WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
			runID).Scan(&errText); err != nil {
			t.Fatalf("查询 suspension.error: %v", err)
		}
		if !strings.Contains(errText, "snapshot exploded") {
			t.Fatalf("suspension.error = %q, 想要包含 snapshot exploded", errText)
		}
	})

	t.Run("snapshot crash fails", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		h.clock.Advance(testConfig().IdleGrace + time.Second)
		h.provider.SetCrashed(sandboxprovider.ActorName(runID))
		if err := runcontroller.TrySuspendNow(h.controller(), runID); err != nil {
			t.Fatalf("TrySuspendNow: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "crashed") {
			t.Fatalf("FailureReason = %q, 想要包含 crashed", detail.FailureReason)
		}
	})

	t.Run("restore failure fails", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		h.suspendNow(runID)
		h.provider.FailNext(fakeprovider.OpRestore, errors.New("restore exploded"))
		if _, err := h.appendInbox(runID, "wake"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "restore exploded") {
			t.Fatalf("FailureReason = %q, 想要包含 restore exploded", detail.FailureReason)
		}
	})

	t.Run("hello timeout fails", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		h.suspendNow(runID)
		w.SetThawConnects(false)
		if _, err := h.appendInbox(runID, "wake"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitRestoreReturned(runID)

		h.clock.Advance(testConfig().HelloTimeout + time.Second)
		if err := runcontroller.ExpireHelloNow(h.controller(), runID); err != nil {
			t.Fatalf("ExpireHelloNow: %v", err)
		}
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "hello timeout") {
			t.Fatalf("FailureReason = %q, 想要 hello timeout", detail.FailureReason)
		}
	})
}

// ---------------------------------------------------------------------------
// 四种并发交错，每种两种先后顺序。

// TestE2EStreamClosedVsHello covers “旧流关闭与新流 Hello”：旧流晚于新流
// 关闭时不把新流上的 delivering 退回，消息在新流上只投递一次。
func TestE2EStreamClosedVsHello(t *testing.T) {
	t.Parallel()

	t.Run("new hello first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _, cancel1 := stepToBoundaryCancelable(t, h)

		if _, err := appendHuman(h.ctrl, context.Background(), runID, "second"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

		gate := newLockGate("hello")
		h.useGate(t, gate)

		stream2, _ := openStreamWithCancel(t, h)
		rec := recordControl(stream2)
		sendDone := make(chan error, 1)
		go func() {
			sendDone <- stream2.Send(helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
		}()
		gate.waitEntered(t)

		cancel1()
		awaitLockWaiter(t, h.pool)
		gate.open()
		if err := <-sendDone; err != nil {
			t.Fatalf("新流发送 Hello: %v", err)
		}

		eventually(t, "新流收到 Deliver", func() bool { return rec.countDeliverSeq(2) > 0 })
		awaitNoLockWaiter(t, h.pool)
		rowLockBarrier(t, h.ctrl, runID)

		state, substate := h.state(runID)
		if state != runcontroller.StateActive || substate != runcontroller.SubstateDelivering {
			t.Fatalf("旧流关闭后状态 = %s/%s, 想要 active/delivering", state, substate)
		}
		if got := rec.countDeliverSeq(2); got != 1 {
			t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("old close first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _, cancel1 := stepToBoundaryCancelable(t, h)

		if _, err := appendHuman(h.ctrl, context.Background(), runID, "second"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

		gate := newLockGate("stream_closed")
		h.useGate(t, gate)

		cancel1()
		gate.waitEntered(t)
		gate.open()
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

		stream2, _ := openStreamWithCancel(t, h)
		rec := recordControl(stream2)
		send(t, stream2, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))

		eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
		eventually(t, "新流收到 Deliver", func() bool { return rec.countDeliverSeq(2) > 0 })
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
		if got := rec.countDeliverSeq(2); got != 1 {
			t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
		}
	})
}

// TestE2EHelloVsAppend covers “新流 Hello 与 AppendInbox”：每条流上
// HelloAck 先于 Deliver，消息只投递一次。
func TestE2EHelloVsAppend(t *testing.T) {
	t.Parallel()

	t.Run("hello first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _, _ := stepToBoundaryCancelable(t, h)

		gate := newLockGate("hello")
		h.useGate(t, gate)

		stream2, _ := openStreamWithCancel(t, h)
		rec := recordControl(stream2)
		sendDone := make(chan error, 1)
		go func() {
			sendDone <- stream2.Send(helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
		}()
		gate.waitEntered(t)

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := appendHuman(h.ctrl, context.Background(), runID, "second")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		awaitLockWaiter(t, h.pool)

		gate.open()
		if err := <-sendDone; err != nil {
			t.Fatalf("发送 Hello: %v", err)
		}
		res := <-appendDone
		if res.err != nil || res.seq != 2 {
			t.Fatalf("AppendInbox: seq=%d err=%v, 想要 2", res.seq, res.err)
		}

		eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
		eventually(t, "新流收到 Deliver", func() bool { return rec.countDeliverSeq(2) > 0 })
		if !rec.firstFrameIsHelloAck() {
			t.Fatal("新流上的第一帧不是 HelloAck")
		}
		if got := rec.countDeliverSeq(2); got != 1 {
			t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
		}
	})

	t.Run("append first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, _, _ := stepToBoundaryCancelable(t, h)

		gate := newLockGate("append")
		h.useGate(t, gate)

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := appendHuman(h.ctrl, context.Background(), runID, "second")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		gate.waitEntered(t)

		stream2, _ := openStreamWithCancel(t, h)
		rec := recordControl(stream2)
		sendDone := make(chan error, 1)
		go func() {
			sendDone <- stream2.Send(helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
		}()
		awaitLockWaiter(t, h.pool)

		gate.open()
		res := <-appendDone
		if res.err != nil || res.seq != 2 {
			t.Fatalf("AppendInbox: seq=%d err=%v, 想要 2", res.seq, res.err)
		}
		if err := <-sendDone; err != nil {
			t.Fatalf("发送 Hello: %v", err)
		}

		eventually(t, "新流收到 HelloAck", rec.helloAckSeen)
		eventually(t, "新流收到 Deliver", func() bool { return rec.countDeliverSeq(2) > 0 })
		if !rec.firstFrameIsHelloAck() {
			t.Fatal("新流上的第一帧不是 HelloAck")
		}
		if got := rec.countDeliverSeq(2); got != 1 {
			t.Fatalf("新流上消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
		}
	})
}

// TestE2EAppendVsTurnEnded covers “AppendInbox 与 turn_ended”：两种顺序下
// 该消息都恰好投递一次。
func TestE2EAppendVsTurnEnded(t *testing.T) {
	t.Parallel()

	setupInTurn := func(t *testing.T, h *harness) (string, agentprotocolv1.AgentStream_ConnectClient, *controlRecorder) {
		t.Helper()
		runID := h.createRun("first")
		h.waitCalls(fakeprovider.OpProvision, 1)

		stream, _ := openStreamWithCancel(t, h)
		rec := recordControl(stream)
		send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
		send(t, stream, acceptedFrame(runID, 1))
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
		send(t, stream, turnStartedFrame(runID, "turn-1", "delivery-1"))
		return runID, stream, rec
	}

	checkDelivered := func(t *testing.T, h *harness, runID string, rec *controlRecorder) {
		t.Helper()
		eventually(t, "消息 2 投递", func() bool { return rec.countDeliverSeq(2) > 0 })
		if got := rec.countDeliverSeq(2); got != 1 {
			t.Fatalf("消息 2 的 Deliver = %d 次, 想要恰好 1 次", got)
		}
		// The Deliver is enqueued before the transaction commits, so wait for
		// the committed state rather than reading it once.
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)
	}

	t.Run("append first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, stream, rec := setupInTurn(t, h)

		gate := newLockGate("append")
		h.useGate(t, gate)

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := appendHuman(h.ctrl, context.Background(), runID, "second")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		gate.waitEntered(t)

		endDone := make(chan error, 1)
		go func() { endDone <- stream.Send(turnEndedFrame(runID, "turn-1")) }()
		awaitLockWaiter(t, h.pool)

		gate.open()
		res := <-appendDone
		if res.err != nil || res.seq != 2 {
			t.Fatalf("AppendInbox: seq=%d err=%v, 想要 2", res.seq, res.err)
		}
		if err := <-endDone; err != nil {
			t.Fatalf("发送 turn_ended: %v", err)
		}
		checkDelivered(t, h, runID, rec)
	})

	t.Run("turn_ended first", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, testConfig())
		runID, stream, rec := setupInTurn(t, h)

		gate := newLockGate("turn_ended")
		h.useGate(t, gate)

		endDone := make(chan error, 1)
		go func() { endDone <- stream.Send(turnEndedFrame(runID, "turn-1")) }()
		gate.waitEntered(t)

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := appendHuman(h.ctrl, context.Background(), runID, "second")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		awaitLockWaiter(t, h.pool)

		gate.open()
		if err := <-endDone; err != nil {
			t.Fatalf("发送 turn_ended: %v", err)
		}
		res := <-appendDone
		if res.err != nil || res.seq != 2 {
			t.Fatalf("AppendInbox: seq=%d err=%v, 想要 2", res.seq, res.err)
		}
		checkDelivered(t, h, runID, rec)
	})
}

// ---------------------------------------------------------------------------
// 自发回合与 Exit。

// TestE2ESpontaneousTurn covers the “自发回合” section: a turn the agent
// starts by itself writes a turn record with an empty delivery_id, and a
// spontaneous turn racing AppendInbox passes through delivering and in_turn.
func TestE2ESpontaneousTurn(t *testing.T) {
	t.Parallel()

	t.Run("at boundary", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		before := len(w.Turns())
		w.StartSpontaneousTurn()
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

		eventually(t, "假看守进程记录自发回合", func() bool { return len(w.Turns()) == before+1 })
		turn := w.Turns()[before]
		if turn.DeliveryID != "" {
			t.Fatalf("自发回合的 delivery_id = %q, 想要空", turn.DeliveryID)
		}
		if got := h.turnDeliveryID(runID, turn.TurnID); got != "" {
			t.Fatalf("库内回合的 delivery_id = %q, 想要空", got)
		}

		w.EndTurn("done", false)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		assertDeliverCounts(t, w, 1)
	})

	t.Run("append first then spontaneous", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		appendGate := newLockGate("append")
		acceptedGate := newLockGate("accepted")
		turnEndedGate := newLockGate("turn_ended")
		gates := []*lockGate{appendGate, acceptedGate, turnEndedGate}
		h.setHook(func(op, runID string) {
			for _, g := range gates {
				g.hook(op, runID)
			}
		})
		t.Cleanup(func() {
			for _, g := range gates {
				g.open()
			}
			h.setHook(nil)
		})

		appendDone := make(chan appendResult, 1)
		go func() {
			seq, err := h.appendInbox(runID, "second")
			appendDone <- appendResult{seq: seq, err: err}
		}()
		appendGate.waitEntered(t)

		// The append holds the row lock; the warden is still at its turn
		// boundary, so this turn starts spontaneously and its turn_started
		// waits behind the append.
		w.StartSpontaneousTurn()

		appendGate.open()

		acceptedGate.waitEntered(t)
		state, substate := h.state(runID)
		if state != runcontroller.StateActive || substate != runcontroller.SubstateDelivering {
			t.Fatalf("Accepted 之前状态 = %s/%s, 想要 active/delivering", state, substate)
		}
		acceptedGate.open()

		turnEndedGate.waitEntered(t)
		state, substate = h.state(runID)
		if state != runcontroller.StateActive || substate != runcontroller.SubstateInTurn {
			t.Fatalf("turn_ended 之前状态 = %s/%s, 想要 active/in_turn", state, substate)
		}
		turnEndedGate.open()

		res := <-appendDone
		if res.err != nil || res.seq != 2 {
			t.Fatalf("AppendInbox: seq=%d err=%v, 想要 2", res.seq, res.err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 2)
		assertWritten(t, w.Written(), 2)
		assertDeliverCounts(t, w, 2)

		found := false
		for _, turn := range w.Turns() {
			if turn.DeliveryID == "" {
				found = true
				if got := h.turnDeliveryID(runID, turn.TurnID); got != "" {
					t.Fatalf("库内自发回合 %s 的 delivery_id = %q, 想要空", turn.TurnID, got)
				}
			}
		}
		if !found {
			t.Fatalf("假看守进程没有记录自发回合: %+v", w.Turns())
		}
	})
}

// TestE2EExitStates covers Exit in boundary, delivering and in_turn: the run
// becomes failed in all three.
func TestE2EExitStates(t *testing.T) {
	t.Parallel()

	assertExited := func(t *testing.T, h *e2eHarness, runID string) {
		t.Helper()
		detail := h.waitState(runID, runcontroller.StateFailed, runcontroller.SubstateNone)
		if !strings.Contains(detail.FailureReason, "agent exited") {
			t.Fatalf("FailureReason = %q, 想要包含 agent exited", detail.FailureReason)
		}
	}

	t.Run("boundary", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		w.Exit()
		assertExited(t, h, runID)
	})

	t.Run("in_turn", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		w.StartSpontaneousTurn()
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
		w.Exit()
		assertExited(t, h, runID)
	})

	t.Run("delivering", func(t *testing.T) {
		t.Parallel()
		h := newE2E(t, testConfig())
		runID := h.createRun("first")
		w := h.warden(runID)
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)
		h.waitWritten(w, 1)

		w.DropNext(fakewarden.DropDeliver)
		if _, err := h.appendInbox(runID, "second"); err != nil {
			t.Fatalf("AppendInbox: %v", err)
		}
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateDelivering)

		w.Exit()
		assertExited(t, h, runID)
	})
}
