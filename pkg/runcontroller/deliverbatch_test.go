package runcontroller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

// deliverBatchLimit mirrors the documented 1 MiB bound on one Deliver batch.
const deliverBatchLimit = 1 << 20

// delivers returns the Deliver frames recorded on the stream, in order.
func (r *controlRecorder) delivers() []*agentprotocolv1.Deliver {
	var out []*agentprotocolv1.Deliver
	for _, f := range r.snapshot() {
		if d := f.GetDeliver(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// closeError returns the error that ended the stream, or nil while it is open.
func (r *controlRecorder) closeError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// suspensionHello reads hello_at and hello_ms of the latest suspension record.
func (h *harness) suspensionHello(runID string) (*time.Time, *int64) {
	h.t.Helper()
	var (
		helloAt *time.Time
		helloMS *int64
	)
	if err := h.pool.QueryRow(context.Background(),
		`SELECT hello_at, hello_ms FROM suspension
		 WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`, runID).Scan(&helloAt, &helloMS); err != nil {
		h.t.Fatalf("查询 hello_at/hello_ms: %v", err)
	}
	return helloAt, helloMS
}

// acceptedFrameFor echoes a Deliver's delivery_id and through_seq, as a real
// warden does.
func acceptedFrameFor(runID string, d *agentprotocolv1.Deliver) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: runID,
		Body: &agentprotocolv1.WardenFrame_Accepted{
			Accepted: &agentprotocolv1.Accepted{
				DeliveryId: d.GetDeliveryId(),
				ThroughSeq: d.GetThroughSeq(),
			},
		},
	}
}

// waitDeliver waits for the n-th Deliver (1-based) and returns it.
func waitDeliver(t *testing.T, rec *controlRecorder, n int) *agentprotocolv1.Deliver {
	t.Helper()

	var got *agentprotocolv1.Deliver
	eventually(t, "Deliver 帧", func() bool {
		ds := rec.delivers()
		if len(ds) < n {
			return false
		}
		got = ds[n-1]
		return true
	})
	return got
}

// inTurnRun walks a fresh run to active/in_turn on its first stream: the first
// message is delivered and accepted, and the turn it opened is recorded. The
// returned recorder drains every frame the control plane sends on the stream.
func inTurnRun(t *testing.T, h *harness) (string, agentprotocolv1.AgentStream_ConnectClient, *controlRecorder) {
	t.Helper()

	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream := h.openStream()
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	first := waitDeliver(t, rec, 1)
	send(t, stream, acceptedFrameFor(runID, first))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
	send(t, stream, turnStartedFrame(runID, "turn-1", first.GetDeliveryId()))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	return runID, stream, rec
}

// subscriptionProto is the protocol message the controller must build for sc:
// every field is mapped and OccurredAt becomes a Timestamp.
func subscriptionProto(sc *runcontroller.SubscriptionCompletion) *agentprotocolv1.SubscriptionCompleted {
	return &agentprotocolv1.SubscriptionCompleted{
		SubId:              sc.SubID,
		System:             sc.System,
		Event:              sc.Event,
		SubscriptionStatus: sc.SubscriptionStatus,
		Terminal:           sc.Terminal,
		Correlation:        sc.Correlation,
		OccurredAt:         timestamppb.New(sc.OccurredAt),
		Source:             sc.Source,
		Detail:             sc.Detail,
		DetailTruncated:    sc.DetailTruncated,
	}
}

// subscriptionProtoCost is the number of bytes one subscription completion
// accounts for against the batch limit: its detail plus the encoded size of
// the other fields, i.e. the serialized size of the whole message.
func subscriptionProtoCost(sc *runcontroller.SubscriptionCompletion) int {
	return proto.Size(subscriptionProto(sc))
}

// subscriptionEventWithDetail returns a subscription completion event with the
// given dedup key and detail.
func subscriptionEventWithDetail(dedupKey, detail string) runcontroller.InboxEvent {
	sc := testSubscriptionCompletion()
	sc.Detail = detail
	return runcontroller.InboxEvent{
		Kind:         runcontroller.KindSubscriptionCompleted,
		EventID:      "evt-" + dedupKey,
		DedupKey:     dedupKey,
		Subscription: sc,
	}
}

// boundaryRun walks a fresh run to active/boundary on its first stream: the
// first message is delivered, accepted and its turn ends.
func boundaryRun(t *testing.T, h *harness) (string, agentprotocolv1.AgentStream_ConnectClient) {
	t.Helper()

	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	stream := h.openStream()
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))
	first := waitDeliver(t, rec, 1)
	send(t, stream, acceptedFrameFor(runID, first))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	send(t, stream, turnStartedFrame(runID, "turn-1", first.GetDeliveryId()))
	send(t, stream, turnEndedFrame(runID, "turn-1"))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateBoundary)

	return runID, stream
}

// TestDeliverBatchThroughSeqMatchesFrame covers a batch truncated by the 1 MiB
// limit. The reserved send position must equal the through_seq carried in the
// frame, because the warden echoes that frame value in Accepted. With the
// mismatch the control plane rejected the Accepted and closed the stream, and
// the remaining messages were never delivered on that stream.
func TestDeliverBatchThroughSeqMatchesFrame(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID := h.createRun("first")
	h.waitCalls(fakeprovider.OpProvision, 1)

	// One short message plus 16 full-size ones: 1 + 16*64 KiB exceeds the
	// 1 MiB batch bound, while 1 + 15*64 KiB fits, so the first batch stops
	// before the last message.
	const wantMessages = 17
	for i := 0; i < wantMessages-1; i++ {
		text := strings.Repeat(string(rune('a'+i)), runcontroller.MaxTextBytes)
		if _, err := appendHuman(h.ctrl, context.Background(), runID, text); err != nil {
			t.Fatalf("AppendInbox %d: %v", i+2, err)
		}
	}

	stream := h.openStream()
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_STARTING, "", 0, nil))

	first := waitDeliver(t, rec, 1)
	if got := len(first.GetBatch()); got != 16 {
		t.Fatalf("第一批消息数 = %d, 想要 16（1 MiB 上限内）", got)
	}
	lastSeq := first.GetBatch()[len(first.GetBatch())-1].GetInboxSeq()
	if first.GetThroughSeq() != lastSeq {
		t.Fatalf("第一批 through_seq = %d, 批次最后一条 = %d", first.GetThroughSeq(), lastSeq)
	}

	// Echo the frame's delivery_id and through_seq, as a real warden does.
	send(t, stream, acceptedFrameFor(runID, first))
	waitInTurnOrClosed(t, h, rec, runID)

	// End the turn so the remaining message is delivered in the next batch.
	send(t, stream, turnStartedFrame(runID, "turn-1", "delivery-1"))
	send(t, stream, turnEndedFrame(runID, "turn-1"))

	second := waitDeliver(t, rec, 2)
	if got := second.GetThroughSeq(); got != wantMessages {
		t.Fatalf("第二批 through_seq = %d, 想要 %d", got, wantMessages)
	}
	if got := len(second.GetBatch()); got != 1 {
		t.Fatalf("第二批消息数 = %d, 想要 1", got)
	}
	send(t, stream, acceptedFrameFor(runID, second))
	h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)

	// Every message was delivered exactly once across the two batches.
	seqs := rec.deliverSeqs()
	if len(seqs) != wantMessages {
		t.Fatalf("投递的消息 = %d 条, 想要 %d 条: %v", len(seqs), wantMessages, seqs)
	}
	seen := make(map[int64]int, wantMessages)
	for _, seq := range seqs {
		seen[seq]++
	}
	for seq := int64(1); seq <= wantMessages; seq++ {
		if seen[seq] != 1 {
			t.Fatalf("消息 %d 投递 %d 次, 想要恰好 1 次", seq, seen[seq])
		}
	}
}

// waitInTurnOrClosed waits for active/in_turn but fails as soon as the stream
// closes, so the through_seq mismatch surfaces as a direct error.
func waitInTurnOrClosed(t *testing.T, h *harness, rec *controlRecorder, runID string) {
	t.Helper()

	deadline := time.Now().Add(testTimeout)
	for {
		if err := rec.closeError(); err != nil {
			t.Fatalf("第一批 Accepted 后流被关闭: %v", err)
		}
		state, substate := h.state(runID)
		if state == runcontroller.StateActive && substate == runcontroller.SubstateInTurn {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("超时等待 active/in_turn；实际 %s/%s", state, substate)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHelloBeforeRestoreLeavesHelloMSUnmeasured covers a Hello that arrives
// before RestoreSnapshot returns. No restore return moment exists yet, so
// hello_ms has no measured value and must stay NULL instead of being recorded
// as zero.
func TestHelloBeforeRestoreLeavesHelloMSUnmeasured(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, _ := boundaryRun(t, h)
	h.clock.Advance(testConfig().IdleGrace + time.Second)
	if err := runcontroller.TrySuspendNow(h.ctrl, runID); err != nil {
		t.Fatalf("TrySuspendNow: %v", err)
	}
	h.waitState(runID, runcontroller.StateSuspended, runcontroller.SubstateSnapshotted)

	release := h.provider.Hold(fakeprovider.OpRestore)
	t.Cleanup(release)

	if _, err := appendHuman(h.ctrl, context.Background(), runID, "wake"); err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	h.waitState(runID, runcontroller.StateResuming, runcontroller.SubstateSnapshotted)
	h.waitCalls(fakeprovider.OpRestore, 1)

	stream := h.openStream()
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))
	eventually(t, "HelloAck", rec.helloAckSeen)

	helloAt, helloMS := h.suspensionHello(runID)
	if helloAt == nil {
		t.Fatal("Hello 早于 RestoreSnapshot 返回时没有记录 hello_at")
	}
	if helloMS != nil {
		t.Fatalf("Hello 早于 RestoreSnapshot 返回时 hello_ms = %d, 想要未测得（NULL）", *helloMS)
	}

	// Once restore returns, the value stays unmeasured: Hello came first, so
	// the interval between the two moments does not exist.
	release()
	h.waitRestoreReturned(runID)
	_, helloMS = h.suspensionHello(runID)
	if helloMS != nil {
		t.Fatalf("RestoreSnapshot 返回后 hello_ms = %d, 想要保持未测得（NULL）", *helloMS)
	}
}

// TestDeliverMixedInboxKinds covers “人类消息与订阅完成事件交替写入，同一批
// Deliver 按 inbox_seq 排列两种事件”. Both kinds are appended while the run is
// in a turn, so neither is delivered on its own; the turn end delivers one
// batch in inbox order.
func TestDeliverMixedInboxKinds(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testConfig())
	runID, stream, rec := inTurnRun(t, h)
	ctx := context.Background()

	texts := []string{"human-1", "human-2"}
	dedupKeys := []string{"mixed-1", "mixed-2"}
	for i := range texts {
		if _, err := appendHuman(h.ctrl, ctx, runID, texts[i]); err != nil {
			t.Fatalf("AppendInbox 人类消息 %d: %v", i, err)
		}
		if _, _, err := h.ctrl.AppendInbox(ctx, runID, testSubscriptionEvent(dedupKeys[i])); err != nil {
			t.Fatalf("AppendInbox 订阅完成 %d: %v", i, err)
		}
	}

	send(t, stream, turnEndedFrame(runID, "turn-1"))

	d := waitDeliver(t, rec, 2)
	batch := d.GetBatch()
	if len(batch) != 2*len(texts) {
		t.Fatalf("同一批消息数 = %d, 想要 %d", len(batch), 2*len(texts))
	}
	for i, m := range batch {
		if want := int64(2 + i); m.GetInboxSeq() != want {
			t.Fatalf("第 %d 条 inbox_seq = %d, 想要 %d", i, m.GetInboxSeq(), want)
		}
	}
	for i := range texts {
		human := batch[2*i]
		if got := human.GetText(); got != texts[i] {
			t.Fatalf("第 %d 条文本 = %q, 想要 %q", 2*i, got, texts[i])
		}
		if human.GetSubscriptionCompleted() != nil {
			t.Fatalf("第 %d 条同时带了 subscription_completed", 2*i)
		}

		sub := batch[2*i+1].GetSubscriptionCompleted()
		if sub == nil {
			t.Fatalf("第 %d 条落到了 text，想要 subscription_completed", 2*i+1)
		}
		if want := subscriptionProto(testSubscriptionCompletion()); !proto.Equal(sub, want) {
			t.Fatalf("第 %d 条订阅完成 = %v, 想要 %v", 2*i+1, sub, want)
		}
	}
	if want := int64(2*len(texts) + 1); d.GetThroughSeq() != want {
		t.Fatalf("through_seq = %d, 想要 %d", d.GetThroughSeq(), want)
	}
}

// TestDeliverBatchSubscriptionDetailLimit covers “含大 detail 的事件按上限
// 分批”. A subscription completion counts its detail plus the other fields
// against the shared 1 MiB limit.
func TestDeliverBatchSubscriptionDetailLimit(t *testing.T) {
	t.Parallel()

	t.Run("subscriptions split at the limit", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, testConfig())
		runID, stream, rec := inTurnRun(t, h)
		ctx := context.Background()

		detail := strings.Repeat("d", runcontroller.MaxDetailBytes)
		unit := subscriptionProtoCost(subscriptionEventWithDetail("unit", detail).Subscription)
		if unit <= 0 || unit >= deliverBatchLimit {
			t.Fatalf("单条事件的成本 = %d, 想要在 (0, %d) 内", unit, deliverBatchLimit)
		}

		// Enough full-size events that the limit is crossed partway through.
		const count = 22
		if count*unit <= deliverBatchLimit {
			t.Fatalf("测试前提不成立：%d 条事件成本 %d 未超过 %d", count, count*unit, deliverBatchLimit)
		}
		var (
			wantFirst int
			total     int
		)
		for wantFirst < count {
			if wantFirst > 0 && total+unit > deliverBatchLimit {
				break
			}
			total += unit
			wantFirst++
		}

		for i := 0; i < count; i++ {
			ev := subscriptionEventWithDetail(fmt.Sprintf("detail-%d", i), detail)
			if _, _, err := h.ctrl.AppendInbox(ctx, runID, ev); err != nil {
				t.Fatalf("AppendInbox %d: %v", i, err)
			}
		}
		send(t, stream, turnEndedFrame(runID, "turn-1"))

		first := waitDeliver(t, rec, 2)
		if got := len(first.GetBatch()); got != wantFirst {
			t.Fatalf("第一批消息数 = %d, 想要 %d（上限内）", got, wantFirst)
		}
		for i, m := range first.GetBatch() {
			if m.GetSubscriptionCompleted() == nil {
				t.Fatalf("第一批第 %d 条不是 subscription_completed", i)
			}
			if want := int64(2 + i); m.GetInboxSeq() != want {
				t.Fatalf("第一批第 %d 条 inbox_seq = %d, 想要 %d", i, m.GetInboxSeq(), want)
			}
		}
		if want := int64(1 + wantFirst); first.GetThroughSeq() != want {
			t.Fatalf("第一批 through_seq = %d, 想要 %d", first.GetThroughSeq(), want)
		}

		send(t, stream, acceptedFrameFor(runID, first))
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
		send(t, stream, turnStartedFrame(runID, "turn-2", first.GetDeliveryId()))
		send(t, stream, turnEndedFrame(runID, "turn-2"))

		second := waitDeliver(t, rec, 3)
		if got, want := len(second.GetBatch()), count-wantFirst; got != want {
			t.Fatalf("第二批消息数 = %d, 想要 %d", got, want)
		}
		if got, want := second.GetBatch()[0].GetInboxSeq(), int64(2+wantFirst); got != want {
			t.Fatalf("第二批首条 inbox_seq = %d, 想要 %d", got, want)
		}
	})

	t.Run("exact limit then one more byte", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, testConfig())
		runID, stream, rec := inTurnRun(t, h)
		ctx := context.Background()

		detail := strings.Repeat("d", runcontroller.MaxDetailBytes)
		unit := subscriptionProtoCost(subscriptionEventWithDetail("unit", detail).Subscription)

		// Fill the batch with full-size subscription completions, then one
		// human message that makes the total exactly the limit.
		n := (deliverBatchLimit - 1) / unit
		for n > 0 && deliverBatchLimit-n*unit > runcontroller.MaxTextBytes {
			n--
		}
		remainder := deliverBatchLimit - n*unit
		if n < 1 || remainder < 1 || remainder > runcontroller.MaxTextBytes {
			t.Fatalf("无法构造恰好填满上限的批次: n=%d remainder=%d", n, remainder)
		}

		for i := 0; i < n; i++ {
			ev := subscriptionEventWithDetail(fmt.Sprintf("exact-%d", i), detail)
			if _, _, err := h.ctrl.AppendInbox(ctx, runID, ev); err != nil {
				t.Fatalf("AppendInbox %d: %v", i, err)
			}
		}
		if _, err := appendHuman(h.ctrl, ctx, runID, strings.Repeat("h", remainder)); err != nil {
			t.Fatalf("AppendInbox 填充消息: %v", err)
		}
		if _, err := appendHuman(h.ctrl, ctx, runID, "x"); err != nil {
			t.Fatalf("AppendInbox 超限消息: %v", err)
		}
		send(t, stream, turnEndedFrame(runID, "turn-1"))

		first := waitDeliver(t, rec, 2)
		if got, want := len(first.GetBatch()), n+1; got != want {
			t.Fatalf("恰好上限的批次消息数 = %d, 想要 %d", got, want)
		}
		if got := n*unit + remainder; got != deliverBatchLimit {
			t.Fatalf("第一批总字节 = %d, 想要 %d", got, deliverBatchLimit)
		}
		if got, want := first.GetThroughSeq(), int64(n+2); got != want {
			t.Fatalf("第一批 through_seq = %d, 想要 %d", got, want)
		}
		last := first.GetBatch()[n]
		if got := len(last.GetText()); got != remainder {
			t.Fatalf("填充消息长度 = %d, 想要 %d", got, remainder)
		}
		if last.GetSubscriptionCompleted() != nil {
			t.Fatal("填充消息被投递成 subscription_completed")
		}

		// The one byte over the limit is delivered in the next batch.
		send(t, stream, acceptedFrameFor(runID, first))
		h.waitState(runID, runcontroller.StateActive, runcontroller.SubstateInTurn)
		send(t, stream, turnStartedFrame(runID, "turn-2", first.GetDeliveryId()))
		send(t, stream, turnEndedFrame(runID, "turn-2"))

		second := waitDeliver(t, rec, 3)
		if got, want := len(second.GetBatch()), 1; got != want {
			t.Fatalf("超限批次消息数 = %d, 想要 %d", got, want)
		}
		if got, want := second.GetBatch()[0].GetInboxSeq(), int64(n+3); got != want {
			t.Fatalf("超限消息 inbox_seq = %d, 想要 %d", got, want)
		}
		if got := second.GetBatch()[0].GetText(); got != "x" {
			t.Fatalf("超限消息文本 = %q, 想要 %q", got, "x")
		}
	})
}

// TestDeliverSubscriptionCompletedAfterRestore covers “恢复后以
// subscription_completed 项投递，字段与写入时一致”.
func TestDeliverSubscriptionCompletedAfterRestore(t *testing.T) {
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

	ev := testSubscriptionEvent("restore-deliver")
	seq, duplicate, err := h.ctrl.AppendInbox(context.Background(), runID, ev)
	if err != nil {
		t.Fatalf("AppendInbox: %v", err)
	}
	if duplicate || seq != 2 {
		t.Fatalf("AppendInbox = (%d, %v), 想要 (2, false)", seq, duplicate)
	}
	h.waitCalls(fakeprovider.OpRestore, 1)

	// The Hello after restore delivers the stored event. It is sent before
	// RestoreSnapshot returns, which the state machine accepts.
	stream := h.openStream()
	rec := recordControl(stream)
	send(t, stream, helloFrame(runID, agentprotocolv1.Phase_PHASE_BOUNDARY, "", 1, nil))

	d := waitDeliver(t, rec, 1)
	if got := len(d.GetBatch()); got != 1 {
		t.Fatalf("恢复后投递的消息数 = %d, 想要 1", got)
	}
	m := d.GetBatch()[0]
	if m.GetInboxSeq() != 2 {
		t.Fatalf("恢复后投递的 inbox_seq = %d, 想要 2", m.GetInboxSeq())
	}
	got := m.GetSubscriptionCompleted()
	if got == nil {
		t.Fatal("恢复后投递的项不是 subscription_completed")
	}
	if !got.GetDetailTruncated() {
		t.Fatal("恢复后投递的 DetailTruncated = false, 想要 true")
	}
	if want := subscriptionProto(ev.Subscription); !proto.Equal(got, want) {
		t.Fatalf("恢复后投递的字段 = %v, 想要 %v", got, want)
	}
}
