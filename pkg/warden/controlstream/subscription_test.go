package controlstream

import (
	"strings"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/warden/agent"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// sampleSubscription 返回一个字段齐全的订阅完成事件，occurred_at 带毫秒以
// 验证按 RFC 3339 写出时截掉小数部分。
func sampleSubscription() *agentprotocolv1.SubscriptionCompleted {
	return &agentprotocolv1.SubscriptionCompleted{
		SubId:              "sub-9",
		System:             "extsim",
		Event:              "job.completed",
		SubscriptionStatus: "completed",
		Terminal:           "succeeded",
		// 键按非字典序写出，且至少四个；未排序的实现在 map 的随机迭代
		// 顺序下几乎不可能恰好与期望文本相等。
		Correlation: map[string]string{
			"workflow_run.id":     "run-7",
			"job.id":              "job-42",
			"pull_request.number": "42",
			"account":             "acme",
		},
		OccurredAt: timestamppb.New(time.Date(2026, 11, 20, 1, 2, 3, 400_000_000, time.UTC)),
		Source:     "webhook",
		Detail:     `{"job":{"id":"job-1"}}`,
	}
}

// subscriptionInbox 构造一条承载订阅完成事件的收件箱消息。
func subscriptionInbox(seq int64, s *agentprotocolv1.SubscriptionCompleted) *agentprotocolv1.InboxMessage {
	return &agentprotocolv1.InboxMessage{
		InboxSeq: seq,
		Content:  &agentprotocolv1.InboxMessage_SubscriptionCompleted{SubscriptionCompleted: s},
	}
}

func TestSubscriptionCompletedText(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agentprotocolv1.SubscriptionCompleted)
		want   string
	}{
		{
			name:   "full fields with sorted correlation",
			mutate: func(*agentprotocolv1.SubscriptionCompleted) {},
			want: "[castworks] Subscription sub-9 completed.\n" +
				"system: extsim\n" +
				"event: job.completed\n" +
				"outcome: succeeded\n" +
				"correlation: account=acme, job.id=job-42, pull_request.number=42, workflow_run.id=run-7\n" +
				"occurred_at: 2026-11-20T01:02:03Z\n" +
				"source: webhook\n" +
				"detail:\n" +
				`{"job":{"id":"job-1"}}`,
		},
		{
			name: "truncated detail adds a final line",
			mutate: func(s *agentprotocolv1.SubscriptionCompleted) {
				s.DetailTruncated = true
			},
			want: "[castworks] Subscription sub-9 completed.\n" +
				"system: extsim\n" +
				"event: job.completed\n" +
				"outcome: succeeded\n" +
				"correlation: account=acme, job.id=job-42, pull_request.number=42, workflow_run.id=run-7\n" +
				"occurred_at: 2026-11-20T01:02:03Z\n" +
				"source: webhook\n" +
				"detail:\n" +
				`{"job":{"id":"job-1"}}` + "\n(detail truncated)",
		},
		{
			name: "empty detail omits the content line",
			mutate: func(s *agentprotocolv1.SubscriptionCompleted) {
				s.Detail = ""
			},
			want: "[castworks] Subscription sub-9 completed.\n" +
				"system: extsim\n" +
				"event: job.completed\n" +
				"outcome: succeeded\n" +
				"correlation: account=acme, job.id=job-42, pull_request.number=42, workflow_run.id=run-7\n" +
				"occurred_at: 2026-11-20T01:02:03Z\n" +
				"source: webhook\n" +
				"detail:",
		},
		{
			name: "empty detail with truncation",
			mutate: func(s *agentprotocolv1.SubscriptionCompleted) {
				s.Detail = ""
				s.DetailTruncated = true
			},
			want: "[castworks] Subscription sub-9 completed.\n" +
				"system: extsim\n" +
				"event: job.completed\n" +
				"outcome: succeeded\n" +
				"correlation: account=acme, job.id=job-42, pull_request.number=42, workflow_run.id=run-7\n" +
				"occurred_at: 2026-11-20T01:02:03Z\n" +
				"source: webhook\n" +
				"detail:\n(detail truncated)",
		},
		{
			name: "empty correlation has no trailing space",
			mutate: func(s *agentprotocolv1.SubscriptionCompleted) {
				s.Correlation = nil
			},
			want: "[castworks] Subscription sub-9 completed.\n" +
				"system: extsim\n" +
				"event: job.completed\n" +
				"outcome: succeeded\n" +
				"correlation:\n" +
				"occurred_at: 2026-11-20T01:02:03Z\n" +
				"source: webhook\n" +
				"detail:\n" +
				`{"job":{"id":"job-1"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleSubscription()
			tc.mutate(s)
			if got := subscriptionCompletedText(s); got != tc.want {
				t.Errorf("subscriptionCompletedText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInboxMessageText(t *testing.T) {
	if got, ok := inboxMessageText(inbox(1, "hello")); !ok || got != "hello" {
		t.Errorf("inboxMessageText(text) = %q, %v; want %q, true", got, ok, "hello")
	}

	s := sampleSubscription()
	want := subscriptionCompletedText(s)
	if got, ok := inboxMessageText(subscriptionInbox(2, s)); !ok || got != want {
		t.Errorf("inboxMessageText(subscription) = %q, %v; want the template text", got, ok)
	}

	if _, ok := inboxMessageText(&agentprotocolv1.InboxMessage{InboxSeq: 3}); ok {
		t.Error("inboxMessageText accepted a message without content")
	}
	if _, ok := inboxMessageText(&agentprotocolv1.InboxMessage{
		InboxSeq: 4,
		Content:  &agentprotocolv1.InboxMessage_SubscriptionCompleted{},
	}); ok {
		t.Error("inboxMessageText accepted a nil subscription_completed")
	}
}

func TestDeliverSubscriptionCompletedIsNotBlank(t *testing.T) {
	s := sampleSubscription()
	s.DetailTruncated = true
	ag, accepted := runDeliverCase(t, []*agentprotocolv1.ControlFrame{
		deliverFrame(testRunID, "d1", subscriptionInbox(1, s)),
	}, 1)

	batches := ag.recordedBatches()
	if len(batches) != 1 {
		t.Fatalf("Deliver called %d times, want 1: %+v", len(batches), batches)
	}
	if len(batches[0].Texts) != 1 {
		t.Fatalf("batch texts = %q, want one subscription text", batches[0].Texts)
	}
	want := subscriptionCompletedText(s)
	if got := batches[0].Texts[0]; got != want {
		t.Errorf("batch text = %q, want %q", got, want)
	}
	if len(accepted) != 1 || accepted[0].GetDeliveryId() != "d1" || accepted[0].GetThroughSeq() != 1 {
		t.Errorf("accepted = %+v, want d1 through 1", accepted)
	}
}

func TestDeliverMixedBatchOrderedByInboxSeq(t *testing.T) {
	s := sampleSubscription()
	subscriptionText := subscriptionCompletedText(s)
	ag, accepted := runDeliverCase(t, []*agentprotocolv1.ControlFrame{
		deliverFrame(testRunID, "d1",
			inbox(1, "first human"),
			subscriptionInbox(2, s),
			inbox(3, "last human"),
		),
	}, 1)

	batches := ag.recordedBatches()
	if len(batches) != 1 {
		t.Fatalf("Deliver called %d times, want 1: %+v", len(batches), batches)
	}
	wantTexts := []string{"first human", subscriptionText, "last human"}
	if len(batches[0].Texts) != len(wantTexts) {
		t.Fatalf("batch texts = %q, want %q", batches[0].Texts, wantTexts)
	}
	for i, want := range wantTexts {
		if got := batches[0].Texts[i]; got != want {
			t.Errorf("batch text %d = %q, want %q", i, got, want)
		}
	}

	// 驱动器以空行连接一批文本，整批因此写成一条用户消息。
	wantMessage := strings.Join(wantTexts, "\n\n")
	if got := strings.Join(batches[0].Texts, "\n\n"); got != wantMessage {
		t.Errorf("joined user message = %q, want %q", got, wantMessage)
	}
	if batches[0].ThroughSeq != 3 {
		t.Errorf("through_seq = %d, want 3", batches[0].ThroughSeq)
	}
	if len(accepted) != 1 || accepted[0].GetThroughSeq() != 3 {
		t.Errorf("accepted = %+v, want through 3", accepted)
	}
}

func TestHandleDeliverRejectsMessageWithoutContent(t *testing.T) {
	ag := newFakeAgent(agent.State{Phase: agent.PhaseBoundary})
	c := New(Config{Address: "127.0.0.1:1", RunID: testRunID})
	d := &agentprotocolv1.Deliver{
		DeliveryId: "d1",
		ThroughSeq: 1,
		Batch:      []*agentprotocolv1.InboxMessage{{InboxSeq: 1}},
	}
	if err := c.handleDeliver(ag, d); err == nil {
		t.Fatal("handleDeliver accepted a message without content")
	}
	if got := ag.recordedBatches(); len(got) != 0 {
		t.Fatalf("Deliver called %d times, want 0: %+v", len(got), got)
	}
}
