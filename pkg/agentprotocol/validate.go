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

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
)

// maxTextBytes is the largest text the protocol allows in one message.
//
// It has the same meaning as runcontroller.MaxTextBytes and the two values
// must stay equal. This package cannot import runcontroller to share
// the constant, because that would make runcontroller depend on agentprotocol
// in reverse; keep the two definitions in sync by hand.
const maxTextBytes = 64 << 10

// maxDetailBytes is the largest detail the protocol allows in one
// subscription completion event.
const maxDetailBytes = 48 << 10

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
		return validateAccepted(body.Accepted)
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
	if err := validateStringSet("hello bg_jobs", h.GetBgJobs()); err != nil {
		return err
	}
	return nil
}

// validateStringSet checks that every element of values is non-empty and that
// no element repeats. Order is not checked.
func validateStringSet(what string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, v := range values {
		if v == "" {
			return fmt.Errorf("agentprotocol: %s entry %d is empty", what, i)
		}
		if _, ok := seen[v]; ok {
			return fmt.Errorf("agentprotocol: %s entry %d repeats %q", what, i, v)
		}
		seen[v] = struct{}{}
	}
	return nil
}

func validateAgentEvent(e *agentprotocolv1.AgentEvent) error {
	if e == nil {
		return errors.New("agentprotocol: event is nil")
	}

	switch kind := e.GetKind().(type) {
	case *agentprotocolv1.AgentEvent_TurnStarted:
		if kind.TurnStarted == nil {
			return errors.New("agentprotocol: event has an empty turn_started kind")
		}
		if e.GetTurnId() == "" {
			return errors.New("agentprotocol: event has an empty turn_id")
		}
		return nil
	case *agentprotocolv1.AgentEvent_TurnEnded:
		if kind.TurnEnded == nil {
			return errors.New("agentprotocol: event has an empty turn_ended kind")
		}
		if e.GetTurnId() == "" {
			return errors.New("agentprotocol: event has an empty turn_id")
		}
		return nil
	case *agentprotocolv1.AgentEvent_BgJobsChanged:
		// turn_id may be empty at a turn boundary, so it is not checked.
		if kind.BgJobsChanged == nil {
			return errors.New("agentprotocol: event has an empty bg_jobs_changed kind")
		}
		return validateStringSet("bg_jobs_changed job_ids", kind.BgJobsChanged.GetJobIds())
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

func validateAccepted(a *agentprotocolv1.Accepted) error {
	if a == nil {
		return errors.New("agentprotocol: accepted is nil")
	}
	if a.GetDeliveryId() == "" {
		return errors.New("agentprotocol: accepted has an empty delivery_id")
	}
	if a.GetThroughSeq() < 1 {
		return fmt.Errorf("agentprotocol: accepted through_seq %d, must be at least 1",
			a.GetThroughSeq())
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
		switch content := msg.GetContent().(type) {
		case *agentprotocolv1.InboxMessage_Text:
			if len(content.Text) > maxTextBytes {
				return fmt.Errorf("agentprotocol: deliver batch entry %d text is %d bytes, over the %d byte limit",
					i, len(content.Text), maxTextBytes)
			}
		case *agentprotocolv1.InboxMessage_SubscriptionCompleted:
			if err := validateSubscriptionCompleted(content.SubscriptionCompleted); err != nil {
				return fmt.Errorf("agentprotocol: deliver batch entry %d: %w", i, err)
			}
		default:
			return fmt.Errorf("agentprotocol: deliver batch entry %d has neither text nor subscription_completed", i)
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

// validateSubscriptionCompleted checks the required fields and the detail
// limit of one subscription completion event. correlation and detail may be
// empty: a subscription always carries correlation keys in practice, but the
// document sets no per-field rule for it, and a completion event may have no
// detail. detail_truncated is informational and is not cross-checked.
func validateSubscriptionCompleted(s *agentprotocolv1.SubscriptionCompleted) error {
	if s == nil {
		return errors.New("subscription_completed is nil")
	}

	required := []struct {
		name  string
		value string
	}{
		{"sub_id", s.GetSubId()},
		{"system", s.GetSystem()},
		{"event", s.GetEvent()},
		{"subscription_status", s.GetSubscriptionStatus()},
		{"terminal", s.GetTerminal()},
		{"source", s.GetSource()},
	}
	for _, f := range required {
		if f.value == "" {
			return fmt.Errorf("subscription_completed has an empty %s", f.name)
		}
	}
	if s.GetOccurredAt() == nil {
		return errors.New("subscription_completed has no occurred_at")
	}
	if err := s.GetOccurredAt().CheckValid(); err != nil {
		return fmt.Errorf("subscription_completed has an invalid occurred_at: %w", err)
	}
	if len(s.GetDetail()) > maxDetailBytes {
		return fmt.Errorf("subscription_completed detail is %d bytes, over the %d byte limit",
			len(s.GetDetail()), maxDetailBytes)
	}
	return nil
}
