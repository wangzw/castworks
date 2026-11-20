package agentprotocol

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
)

// textLimitBytes is the protocol's text limit. It is written out here on
// purpose instead of reusing the implementation's constant, so that a wrong
// constant in the implementation fails the test instead of redefining it.
const textLimitBytes = 65536

// detailLimitBytes is the protocol's detail limit, also written out on purpose.
const detailLimitBytes = 49152

// textMessage builds an inbox message carrying a human message text.
func textMessage(seq int64, text string) *agentprotocolv1.InboxMessage {
	return &agentprotocolv1.InboxMessage{
		InboxSeq: seq,
		Content:  &agentprotocolv1.InboxMessage_Text{Text: text},
	}
}

// subscriptionMessage builds an inbox message carrying a subscription
// completion event.
func subscriptionMessage(seq int64, s *agentprotocolv1.SubscriptionCompleted) *agentprotocolv1.InboxMessage {
	return &agentprotocolv1.InboxMessage{
		InboxSeq: seq,
		Content:  &agentprotocolv1.InboxMessage_SubscriptionCompleted{SubscriptionCompleted: s},
	}
}

func validSubscriptionCompleted() *agentprotocolv1.SubscriptionCompleted {
	return &agentprotocolv1.SubscriptionCompleted{
		SubId:              "sub-9",
		System:             "extsim",
		Event:              "job.completed",
		SubscriptionStatus: "completed",
		Terminal:           "succeeded",
		Correlation:        map[string]string{"job.id": "job-1"},
		OccurredAt:         timestamppb.New(time.Date(2026, 11, 20, 1, 2, 3, 4_000_000, time.UTC)),
		Source:             "webhook",
		Detail:             `{"job":{"id":"job-1"}}`,
		DetailTruncated:    true,
	}
}

// subscriptionFrame builds a Deliver frame with one subscription completion
// event as its only entry.
func subscriptionFrame(seq int64, s *agentprotocolv1.SubscriptionCompleted) *agentprotocolv1.ControlFrame {
	return deliverFrame("delivery-1", seq, subscriptionMessage(seq, s))
}

// subscriptionWith copies the valid completion event and applies mutate, so a
// rejection case changes one field only.
func subscriptionWith(mutate func(*agentprotocolv1.SubscriptionCompleted)) *agentprotocolv1.SubscriptionCompleted {
	s := validSubscriptionCompleted()
	mutate(s)
	return s
}

func deliverFrame(deliveryID string, throughSeq int64, batch ...*agentprotocolv1.InboxMessage) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: deliveryID,
			Batch:      batch,
			ThroughSeq: throughSeq,
		}},
	}
}

func validHelloFrame() *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Hello{
			Hello: &agentprotocolv1.Hello{
				AgentState: &agentprotocolv1.AgentState{
					Phase:  agentprotocolv1.Phase_PHASE_BOUNDARY,
					TurnId: "turn-1",
				},
				LastAcceptedSeq: 2,
				LastTurn: &agentprotocolv1.LastTurn{
					TurnId:  "turn-0",
					Output:  "done",
					IsError: false,
				},
			},
		},
	}
}

// validBgJobsHelloFrame carries an intentionally unsorted background job set:
// order is not part of validation.
func validBgJobsHelloFrame() *agentprotocolv1.WardenFrame {
	frame := validHelloFrame()
	hello := frame.GetHello()
	hello.BgJobs = []string{"job-b", "job-a"}
	return frame
}

// validBgJobsChangedFrame has an empty turn_id, which is valid at a turn
// boundary.
func validBgJobsChangedFrame() *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				Kind: &agentprotocolv1.AgentEvent_BgJobsChanged{
					BgJobsChanged: &agentprotocolv1.BgJobsChanged{
						JobIds: []string{"job-b", "job-a"},
					},
				},
			},
		},
	}
}

func helloFrameWithBgJobs(jobs ...string) *agentprotocolv1.WardenFrame {
	frame := validHelloFrame()
	frame.GetHello().BgJobs = jobs
	return frame
}

func bgJobsChangedFrame(turnID string, jobIDs ...string) *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: turnID,
				Kind: &agentprotocolv1.AgentEvent_BgJobsChanged{
					BgJobsChanged: &agentprotocolv1.BgJobsChanged{JobIds: jobIDs},
				},
			},
		},
	}
}

func validTurnStartedFrame() *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: "turn-1",
				Kind: &agentprotocolv1.AgentEvent_TurnStarted{
					TurnStarted: &agentprotocolv1.TurnStarted{
						DeliveryId: "delivery-1",
						SessionId:  "session-1",
					},
				},
			},
		},
	}
}

func validTurnEndedFrame() *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Event{
			Event: &agentprotocolv1.AgentEvent{
				TurnId: "turn-1",
				Kind: &agentprotocolv1.AgentEvent_TurnEnded{
					TurnEnded: &agentprotocolv1.TurnEnded{
						Output:      "done",
						IsError:     false,
						AgentExited: true,
					},
				},
			},
		},
	}
}

func validAcceptedFrame() *agentprotocolv1.WardenFrame {
	return &agentprotocolv1.WardenFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.WardenFrame_Accepted{
			Accepted: &agentprotocolv1.Accepted{
				DeliveryId: "delivery-1",
				ThroughSeq: 3,
			},
		},
	}
}

func validHelloAckFrame() *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_HelloAck{
			HelloAck: &agentprotocolv1.HelloAck{
				LlmCredential: &agentprotocolv1.LLMCredential{
					Kind:  agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY,
					Value: "secret",
				},
			},
		},
	}
}

func validDeliverFrame() *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_Deliver{
			Deliver: &agentprotocolv1.Deliver{
				DeliveryId: "delivery-1",
				Batch: []*agentprotocolv1.InboxMessage{
					textMessage(1, "first"),
					textMessage(2, "second"),
				},
				ThroughSeq: 2,
			},
		},
	}
}

func validShutdownFrame() *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_Shutdown{
			Shutdown: &agentprotocolv1.Shutdown{Reason: "ended"},
		},
	}
}

func TestValidateWardenFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *agentprotocolv1.WardenFrame
		want  string
	}{
		{name: "hello", frame: validHelloFrame()},
		{name: "turn_started", frame: validTurnStartedFrame()},
		{name: "turn_ended", frame: validTurnEndedFrame()},
		{name: "accepted", frame: validAcceptedFrame()},
		{name: "hello with bg_jobs", frame: validBgJobsHelloFrame()},
		{name: "bg_jobs_changed at a boundary without turn_id", frame: validBgJobsChangedFrame()},
		{name: "bg_jobs_changed during a turn", frame: bgJobsChangedFrame("turn-1", "job-a", "job-b")},
		{
			name:  "empty bg_jobs entry",
			frame: helloFrameWithBgJobs("job-a", ""),
			want:  "hello bg_jobs entry 1 is empty",
		},
		{
			name:  "duplicate bg_jobs entry",
			frame: helloFrameWithBgJobs("job-a", "job-a"),
			want:  `hello bg_jobs entry 1 repeats "job-a"`,
		},
		{
			name:  "empty job_ids entry",
			frame: bgJobsChangedFrame("", "job-a", ""),
			want:  "bg_jobs_changed job_ids entry 1 is empty",
		},
		{
			name:  "duplicate job_ids entry",
			frame: bgJobsChangedFrame("", "job-a", "job-a"),
			want:  `bg_jobs_changed job_ids entry 1 repeats "job-a"`,
		},
		{
			name: "nil bg_jobs_changed",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Event{
					Event: &agentprotocolv1.AgentEvent{
						Kind: &agentprotocolv1.AgentEvent_BgJobsChanged{},
					},
				},
			},
			want: "empty bg_jobs_changed kind",
		},
		{
			name:  "nil frame",
			frame: nil,
			want:  "frame is nil",
		},
		{
			name: "empty run_id",
			frame: &agentprotocolv1.WardenFrame{
				Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{}},
			},
			want: "empty run_id",
		},
		{
			name:  "missing body",
			frame: &agentprotocolv1.WardenFrame{RunId: "run-1"},
			want:  "no body",
		},
		{
			name: "nil hello",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Hello{},
			},
			want: "hello is nil",
		},
		{
			name: "hello without agent_state",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Hello{Hello: &agentprotocolv1.Hello{}},
			},
			want: "no agent_state",
		},
		{
			name: "unspecified phase",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Hello{Hello: &agentprotocolv1.Hello{
					AgentState: &agentprotocolv1.AgentState{},
				}},
			},
			want: "phase is unspecified",
		},
		{
			name: "undefined phase",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Hello{Hello: &agentprotocolv1.Hello{
					AgentState: &agentprotocolv1.AgentState{Phase: agentprotocolv1.Phase(99)},
				}},
			},
			want: "phase 99 is not defined",
		},
		{
			name: "nil event",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Event{},
			},
			want: "event is nil",
		},
		{
			name: "empty turn_id",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{
					Kind: &agentprotocolv1.AgentEvent_TurnStarted{TurnStarted: &agentprotocolv1.TurnStarted{}},
				}},
			},
			want: "empty turn_id",
		},
		{
			name: "turn_ended with empty turn_id",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{
					Kind: &agentprotocolv1.AgentEvent_TurnEnded{TurnEnded: &agentprotocolv1.TurnEnded{}},
				}},
			},
			want: "empty turn_id",
		},
		{
			name: "event without kind",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{TurnId: "turn-1"}},
			},
			want: "no kind",
		},
		{
			name: "nil accepted",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.WardenFrame_Accepted{},
			},
			want: "empty accepted body",
		},
		{
			name: "accepted with empty delivery_id",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{
					ThroughSeq: 3,
				}},
			},
			want: "empty delivery_id",
		},
		{
			name: "accepted with through_seq zero",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{
					DeliveryId: "delivery-1",
					ThroughSeq: 0,
				}},
			},
			want: "through_seq 0, must be at least 1",
		},
		{
			name: "accepted with negative through_seq",
			frame: &agentprotocolv1.WardenFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{
					DeliveryId: "delivery-1",
					ThroughSeq: -1,
				}},
			},
			want: "through_seq -1, must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWardenFrame(tt.frame)
			checkValidation(t, err, tt.want)
		})
	}
}

func TestValidateControlFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *agentprotocolv1.ControlFrame
		want  string
	}{
		{name: "hello_ack", frame: validHelloAckFrame()},
		{name: "deliver", frame: validDeliverFrame()},
		{name: "shutdown", frame: validShutdownFrame()},
		{name: "deliver with subscription_completed", frame: subscriptionFrame(1, validSubscriptionCompleted())},
		{
			name: "deliver with mixed content",
			frame: deliverFrame("delivery-1", 2,
				textMessage(1, "hello"),
				subscriptionMessage(2, validSubscriptionCompleted()),
			),
		},
		{
			name:  "inbox message without content",
			frame: deliverFrame("delivery-1", 1, &agentprotocolv1.InboxMessage{InboxSeq: 1}),
			want:  "neither text nor subscription_completed",
		},
		{
			name: "nil subscription_completed",
			frame: deliverFrame("delivery-1", 1, &agentprotocolv1.InboxMessage{
				InboxSeq: 1,
				Content:  &agentprotocolv1.InboxMessage_SubscriptionCompleted{},
			}),
			want: "subscription_completed is nil",
		},
		{
			name:  "subscription_completed with empty sub_id",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.SubId = "" })),
			want:  "empty sub_id",
		},
		{
			name:  "subscription_completed with empty system",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.System = "" })),
			want:  "empty system",
		},
		{
			name:  "subscription_completed with empty event",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.Event = "" })),
			want:  "empty event",
		},
		{
			name:  "subscription_completed with empty subscription_status",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.SubscriptionStatus = "" })),
			want:  "empty subscription_status",
		},
		{
			name:  "subscription_completed with empty terminal",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.Terminal = "" })),
			want:  "empty terminal",
		},
		{
			name:  "subscription_completed with empty source",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.Source = "" })),
			want:  "empty source",
		},
		{
			name:  "subscription_completed without occurred_at",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) { s.OccurredAt = nil })),
			want:  "no occurred_at",
		},
		{
			name: "subscription_completed with invalid occurred_at",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) {
				s.OccurredAt = &timestamppb.Timestamp{Seconds: 253402300800}
			})),
			want: "invalid occurred_at",
		},
		{
			name: "subscription_completed detail over the limit",
			frame: subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) {
				s.Detail = strings.Repeat("a", detailLimitBytes+1)
			})),
			want: "over the",
		},
		{
			name:  "nil frame",
			frame: nil,
			want:  "frame is nil",
		},
		{
			name: "empty run_id",
			frame: &agentprotocolv1.ControlFrame{
				Body: &agentprotocolv1.ControlFrame_Shutdown{Shutdown: &agentprotocolv1.Shutdown{}},
			},
			want: "empty run_id",
		},
		{
			name:  "missing body",
			frame: &agentprotocolv1.ControlFrame{RunId: "run-1"},
			want:  "no body",
		},
		{
			name: "nil hello_ack",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.ControlFrame_HelloAck{},
			},
			want: "hello_ack is nil",
		},
		{
			name: "hello_ack without credential",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{}},
			},
			want: "no llm_credential",
		},
		{
			name: "unspecified credential kind",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{
					LlmCredential: &agentprotocolv1.LLMCredential{Value: "secret"},
				}},
			},
			want: "kind is unspecified",
		},
		{
			name: "undefined credential kind",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body: &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{
					LlmCredential: &agentprotocolv1.LLMCredential{
						Kind:  agentprotocolv1.LLMCredentialKind(7),
						Value: "secret",
					},
				}},
			},
			want: "kind 7 is not defined",
		},
		{
			name: "nil deliver",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.ControlFrame_Deliver{},
			},
			want: "deliver is nil",
		},
		{
			name:  "empty batch",
			frame: deliverFrame("delivery-1", 1),
			want:  "empty batch",
		},
		{
			name:  "empty delivery_id",
			frame: deliverFrame("", 1, textMessage(1, "first")),
			want:  "empty delivery_id",
		},
		{
			name:  "nil batch entry",
			frame: deliverFrame("delivery-1", 1, nil),
			want:  "entry 0 is nil",
		},
		{
			name:  "inbox_seq below one",
			frame: deliverFrame("delivery-1", 0, textMessage(0, "first")),
			want:  "deliver batch entry 0 has inbox_seq 0, must be at least 1",
		},
		{
			name: "batch not strictly increasing",
			frame: deliverFrame("delivery-1", 1,
				textMessage(1, "first"),
				textMessage(1, "second"),
			),
			want: "strictly increasing",
		},
		{
			name: "inbox_seq decreases",
			frame: deliverFrame("delivery-1", 1,
				textMessage(2, "second"),
				textMessage(1, "first"),
			),
			want: "strictly increasing",
		},
		{
			name: "through_seq is not the last inbox_seq",
			frame: deliverFrame("delivery-1", 1,
				textMessage(1, "first"),
				textMessage(2, "second"),
			),
			want: "does not equal the last inbox_seq",
		},
		{
			name: "through_seq is greater than the last inbox_seq",
			frame: deliverFrame("delivery-1", 3,
				textMessage(1, "first"),
				textMessage(2, "second"),
			),
			want: "does not equal the last inbox_seq",
		},
		{
			name: "text over the limit",
			frame: deliverFrame("delivery-1", 1,
				textMessage(1, strings.Repeat("a", textLimitBytes+1)),
			),
			want: "over the",
		},
		{
			name: "nil shutdown",
			frame: &agentprotocolv1.ControlFrame{
				RunId: "run-1",
				Body:  &agentprotocolv1.ControlFrame_Shutdown{},
			},
			want: "empty shutdown body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateControlFrame(tt.frame)
			checkValidation(t, err, tt.want)
		})
	}
}

// TestDeliverTextLimit checks the exact byte boundary of the text limit.
func TestDeliverTextLimit(t *testing.T) {
	// "𝄞" (U+1D11E) is 4 bytes in UTF-8, so the counts below divide evenly.
	const multibyte = "𝄞"

	t.Run("exactly the limit in bytes passes", func(t *testing.T) {
		text := strings.Repeat(multibyte, textLimitBytes/4)
		if len(text) != textLimitBytes {
			t.Fatalf("test setup: text is %d bytes, want %d", len(text), textLimitBytes)
		}
		frame := deliverFrame("delivery-1", 1, textMessage(1, text))
		if err := ValidateControlFrame(frame); err != nil {
			t.Fatalf("ValidateControlFrame() rejected text of exactly %d bytes: %v", textLimitBytes, err)
		}
	})

	t.Run("fewer runes than the limit but more bytes fails", func(t *testing.T) {
		text := strings.Repeat(multibyte, textLimitBytes/4+1)
		if utf8.RuneCountInString(text) >= textLimitBytes {
			t.Fatalf("test setup: text has %d runes, want fewer than %d", utf8.RuneCountInString(text), textLimitBytes)
		}
		if len(text) <= textLimitBytes {
			t.Fatalf("test setup: text is %d bytes, want more than %d", len(text), textLimitBytes)
		}
		frame := deliverFrame("delivery-1", 1, textMessage(1, text))
		err := ValidateControlFrame(frame)
		if err == nil {
			t.Fatalf("ValidateControlFrame() accepted text of %d bytes", len(text))
		}
		if !strings.Contains(err.Error(), "over the") {
			t.Fatalf("error %q does not contain %q", err, "over the")
		}
	})

	t.Run("second batch entry over the limit fails", func(t *testing.T) {
		frame := deliverFrame("delivery-1", 2,
			textMessage(1, "first"),
			textMessage(2, strings.Repeat("a", textLimitBytes+1)),
		)
		err := ValidateControlFrame(frame)
		if err == nil {
			t.Fatal("ValidateControlFrame() accepted an over-limit second batch entry")
		}
		if !strings.Contains(err.Error(), "over the") {
			t.Fatalf("error %q does not contain %q", err, "over the")
		}
	})
}

// TestSubscriptionDetailLimit checks the exact byte boundary of the detail
// limit.
func TestSubscriptionDetailLimit(t *testing.T) {
	t.Run("exactly the limit passes", func(t *testing.T) {
		frame := subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) {
			s.Detail = strings.Repeat("a", detailLimitBytes)
		}))
		if err := ValidateControlFrame(frame); err != nil {
			t.Fatalf("ValidateControlFrame() rejected detail of exactly %d bytes: %v", detailLimitBytes, err)
		}
	})

	t.Run("one byte over the limit fails", func(t *testing.T) {
		frame := subscriptionFrame(1, subscriptionWith(func(s *agentprotocolv1.SubscriptionCompleted) {
			s.Detail = strings.Repeat("a", detailLimitBytes+1)
		}))
		err := ValidateControlFrame(frame)
		if err == nil {
			t.Fatalf("ValidateControlFrame() accepted detail of %d bytes", detailLimitBytes+1)
		}
		if !strings.Contains(err.Error(), "over the") {
			t.Fatalf("error %q does not contain %q", err, "over the")
		}
	})
}

// TestDeliverTextWireEncoding pins the on-the-wire encoding of a human
// message. The text field stays number 2 after it moves into the content
// oneof, so the bytes are identical to v0.1.0.
func TestDeliverTextWireEncoding(t *testing.T) {
	frame := &agentprotocolv1.ControlFrame{
		RunId: "run-1",
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: "d",
			Batch:      []*agentprotocolv1.InboxMessage{textMessage(1, "hi")},
			ThroughSeq: 1,
		}},
	}

	got, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []byte{
		0x0a, 0x05, 'r', 'u', 'n', '-', '1', // run_id = "run-1"
		0x1a, 0x0d, // deliver, 13 bytes
		0x0a, 0x01, 'd', // delivery_id = "d"
		0x12, 0x06, 0x08, 0x01, 0x12, 0x02, 'h', 'i', // batch[0]: inbox_seq=1, text="hi"
		0x18, 0x01, // through_seq = 1
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire encoding changed:\n got %x\nwant %x", got, want)
	}
}

func checkValidation(t *testing.T, err error, want string) {
	t.Helper()

	if want == "" {
		if err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestWardenFrameRoundTrip(t *testing.T) {
	frames := []struct {
		name  string
		frame *agentprotocolv1.WardenFrame
	}{
		{name: "hello", frame: validHelloFrame()},
		{name: "hello with bg_jobs", frame: validBgJobsHelloFrame()},
		{name: "turn_started", frame: validTurnStartedFrame()},
		{name: "turn_ended", frame: validTurnEndedFrame()},
		{name: "bg_jobs_changed", frame: validBgJobsChangedFrame()},
		{name: "accepted", frame: validAcceptedFrame()},
	}

	for _, tt := range frames {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateWardenFrame(tt.frame); err != nil {
				t.Fatalf("valid frame rejected: %v", err)
			}

			raw, err := proto.Marshal(tt.frame)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got agentprotocolv1.WardenFrame
			if err := proto.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !proto.Equal(tt.frame, &got) {
				t.Fatalf("round trip changed the frame:\n got %v\nwant %v", &got, tt.frame)
			}
			if err := ValidateWardenFrame(&got); err != nil {
				t.Fatalf("decoded frame rejected: %v", err)
			}
		})
	}
}

func TestControlFrameRoundTrip(t *testing.T) {
	frames := []struct {
		name  string
		frame *agentprotocolv1.ControlFrame
	}{
		{name: "hello_ack", frame: validHelloAckFrame()},
		{name: "deliver", frame: validDeliverFrame()},
		{name: "deliver with subscription_completed", frame: subscriptionFrame(1, validSubscriptionCompleted())},
		{name: "shutdown", frame: validShutdownFrame()},
	}

	for _, tt := range frames {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateControlFrame(tt.frame); err != nil {
				t.Fatalf("valid frame rejected: %v", err)
			}

			raw, err := proto.Marshal(tt.frame)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got agentprotocolv1.ControlFrame
			if err := proto.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !proto.Equal(tt.frame, &got) {
				t.Fatalf("round trip changed the frame:\n got %v\nwant %v", &got, tt.frame)
			}
			if err := ValidateControlFrame(&got); err != nil {
				t.Fatalf("decoded frame rejected: %v", err)
			}
		})
	}
}
