package runcontroller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider/fakeprovider"
)

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
