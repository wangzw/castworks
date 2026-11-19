// Package agentprotocol validates the frames exchanged between the warden
// process and the control plane.
//
// Validation is per frame only. Relationships between frames of one stream,
// such as the first frame being Hello or the run_id staying constant, belong
// to the control plane.
package agentprotocol

import (
	"errors"
	"fmt"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
)

// maxTextBytes is the largest text the protocol allows in one message.
//
// It has the same meaning as runcontroller.MaxTextBytes and the two values
// must stay equal. This package cannot import internal/runcontroller to share
// the constant, because that would make runcontroller depend on agentprotocol
// in reverse; keep the two definitions in sync by hand.
const maxTextBytes = 64 << 10

// ValidateWardenFrame checks one frame sent by the warden to the control
// plane. It returns nil when the frame is valid and a descriptive error
// otherwise.
func ValidateWardenFrame(f *agentprotocolv1.WardenFrame) error {
	if f == nil {
		return errors.New("agentprotocol: warden frame is nil")
	}
	if f.GetRunId() == "" {
		return errors.New("agentprotocol: warden frame has an empty run_id")
	}

	switch body := f.GetBody().(type) {
	case *agentprotocolv1.WardenFrame_Hello:
		return validateHello(body.Hello)
	case *agentprotocolv1.WardenFrame_Event:
		return validateAgentEvent(body.Event)
	case *agentprotocolv1.WardenFrame_Accepted:
		if body.Accepted == nil {
			return errors.New("agentprotocol: warden frame has an empty accepted body")
		}
		return nil
	default:
		return errors.New("agentprotocol: warden frame has no body")
	}
}

// ValidateControlFrame checks one frame sent by the control plane to the
// warden. It returns nil when the frame is valid and a descriptive error
// otherwise.
func ValidateControlFrame(f *agentprotocolv1.ControlFrame) error {
	if f == nil {
		return errors.New("agentprotocol: control frame is nil")
	}
	if f.GetRunId() == "" {
		return errors.New("agentprotocol: control frame has an empty run_id")
	}

	switch body := f.GetBody().(type) {
	case *agentprotocolv1.ControlFrame_HelloAck:
		return validateHelloAck(body.HelloAck)
	case *agentprotocolv1.ControlFrame_Deliver:
		return validateDeliver(body.Deliver)
	case *agentprotocolv1.ControlFrame_Shutdown:
		if body.Shutdown == nil {
			return errors.New("agentprotocol: control frame has an empty shutdown body")
		}
		return nil
	default:
		return errors.New("agentprotocol: control frame has no body")
	}
}

func validateHello(h *agentprotocolv1.Hello) error {
	if h == nil {
		return errors.New("agentprotocol: hello is nil")
	}
	if h.GetAgentState() == nil {
		return errors.New("agentprotocol: hello has no agent_state")
	}
	if h.GetAgentState().GetPhase() == agentprotocolv1.Phase_PHASE_UNSPECIFIED {
		return errors.New("agentprotocol: hello agent_state phase is unspecified")
	}
	if _, ok := agentprotocolv1.Phase_name[int32(h.GetAgentState().GetPhase())]; !ok {
		return fmt.Errorf("agentprotocol: hello agent_state phase %d is not defined",
			h.GetAgentState().GetPhase())
	}
	return nil
}

func validateAgentEvent(e *agentprotocolv1.AgentEvent) error {
	if e == nil {
		return errors.New("agentprotocol: event is nil")
	}
	if e.GetTurnId() == "" {
		return errors.New("agentprotocol: event has an empty turn_id")
	}

	switch kind := e.GetKind().(type) {
	case *agentprotocolv1.AgentEvent_TurnStarted:
		if kind.TurnStarted == nil {
			return errors.New("agentprotocol: event has an empty turn_started kind")
		}
		return nil
	case *agentprotocolv1.AgentEvent_TurnEnded:
		if kind.TurnEnded == nil {
			return errors.New("agentprotocol: event has an empty turn_ended kind")
		}
		return nil
	default:
		return errors.New("agentprotocol: event has no kind")
	}
}

func validateHelloAck(a *agentprotocolv1.HelloAck) error {
	if a == nil {
		return errors.New("agentprotocol: hello_ack is nil")
	}
	if a.GetLlmCredential() == nil {
		return errors.New("agentprotocol: hello_ack has no llm_credential")
	}
	if a.GetLlmCredential().GetKind() == agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_UNSPECIFIED {
		return errors.New("agentprotocol: hello_ack llm_credential kind is unspecified")
	}
	if _, ok := agentprotocolv1.LLMCredentialKind_name[int32(a.GetLlmCredential().GetKind())]; !ok {
		return fmt.Errorf("agentprotocol: hello_ack llm_credential kind %d is not defined",
			a.GetLlmCredential().GetKind())
	}
	return nil
}

func validateDeliver(d *agentprotocolv1.Deliver) error {
	if d == nil {
		return errors.New("agentprotocol: deliver is nil")
	}
	if d.GetDeliveryId() == "" {
		return errors.New("agentprotocol: deliver has an empty delivery_id")
	}
	batch := d.GetBatch()
	if len(batch) == 0 {
		return errors.New("agentprotocol: deliver has an empty batch")
	}

	var previous int64
	for i, msg := range batch {
		if msg == nil {
			return fmt.Errorf("agentprotocol: deliver batch entry %d is nil", i)
		}
		if msg.GetInboxSeq() < 1 {
			return fmt.Errorf("agentprotocol: deliver batch entry %d has inbox_seq %d, must be at least 1",
				i, msg.GetInboxSeq())
		}
		if len(msg.GetText()) > maxTextBytes {
			return fmt.Errorf("agentprotocol: deliver batch entry %d text is %d bytes, over the %d byte limit",
				i, len(msg.GetText()), maxTextBytes)
		}
		if i > 0 && msg.GetInboxSeq() <= previous {
			return fmt.Errorf("agentprotocol: deliver batch is not in strictly increasing inbox_seq order at entry %d", i)
		}
		previous = msg.GetInboxSeq()
	}

	if last := batch[len(batch)-1].GetInboxSeq(); d.GetThroughSeq() != last {
		return fmt.Errorf("agentprotocol: deliver through_seq %d does not equal the last inbox_seq %d",
			d.GetThroughSeq(), last)
	}
	return nil
}
