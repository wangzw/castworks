package agentprotocol

import (
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
)

// textLimitBytes is the protocol's text limit. It is written out here on
// purpose instead of reusing the implementation's constant, so that a wrong
// constant in the implementation fails the test instead of redefining it.
const textLimitBytes = 65536

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
					{InboxSeq: 1, Text: "first"},
					{InboxSeq: 2, Text: "second"},
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
			frame: deliverFrame("", 1, &agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"}),
			want:  "empty delivery_id",
		},
		{
			name:  "nil batch entry",
			frame: deliverFrame("delivery-1", 1, nil),
			want:  "entry 0 is nil",
		},
		{
			name:  "inbox_seq below one",
			frame: deliverFrame("delivery-1", 0, &agentprotocolv1.InboxMessage{InboxSeq: 0, Text: "first"}),
			want:  "deliver batch entry 0 has inbox_seq 0, must be at least 1",
		},
		{
			name: "batch not strictly increasing",
			frame: deliverFrame("delivery-1", 1,
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"},
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "second"},
			),
			want: "strictly increasing",
		},
		{
			name: "inbox_seq decreases",
			frame: deliverFrame("delivery-1", 1,
				&agentprotocolv1.InboxMessage{InboxSeq: 2, Text: "second"},
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"},
			),
			want: "strictly increasing",
		},
		{
			name: "through_seq is not the last inbox_seq",
			frame: deliverFrame("delivery-1", 1,
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"},
				&agentprotocolv1.InboxMessage{InboxSeq: 2, Text: "second"},
			),
			want: "does not equal the last inbox_seq",
		},
		{
			name: "through_seq is greater than the last inbox_seq",
			frame: deliverFrame("delivery-1", 3,
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"},
				&agentprotocolv1.InboxMessage{InboxSeq: 2, Text: "second"},
			),
			want: "does not equal the last inbox_seq",
		},
		{
			name: "text over the limit",
			frame: deliverFrame("delivery-1", 1,
				&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: strings.Repeat("a", textLimitBytes+1)},
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
		frame := deliverFrame("delivery-1", 1, &agentprotocolv1.InboxMessage{InboxSeq: 1, Text: text})
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
		frame := deliverFrame("delivery-1", 1, &agentprotocolv1.InboxMessage{InboxSeq: 1, Text: text})
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
			&agentprotocolv1.InboxMessage{InboxSeq: 1, Text: "first"},
			&agentprotocolv1.InboxMessage{InboxSeq: 2, Text: strings.Repeat("a", textLimitBytes+1)},
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
		{name: "turn_started", frame: validTurnStartedFrame()},
		{name: "turn_ended", frame: validTurnEndedFrame()},
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
