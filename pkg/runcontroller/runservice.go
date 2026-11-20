package runcontroller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

// 本文件实现 RunService。每个方法通过 applyEvent 在事务内执行对应的规则；
// 事务提交后立即返回，提交后的提供者调用由 providerQueue 执行，不受调用方
// ctx 取消的影响。

// Create implements RunService.
func (c *Controller) Create(ctx context.Context, prompt string) (string, error) {
	if err := validateText(prompt); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	runID, err := randomID(runIDLength)
	if err != nil {
		return "", fmt.Errorf("runcontroller: generate run id: %w", err)
	}

	now := c.clock.Now()
	row := runRow{
		RunID:            runID,
		State:            StateProvisioning,
		Substate:         SubstateNone,
		ActorName:        sandboxprovider.ActorName(runID),
		NextInboxSeq:     2, // insertRun writes the first message at seq 1
		DeliveredThrough: 0,
		CreatedAt:        now,
	}

	var slot *providerSlot
	err = c.store.inTx(ctx, func(tx *storeTx) error {
		slot = c.calls.reserve(runID)
		return tx.insertRun(row, prompt, now)
	})
	if err != nil {
		committed := false
		var ce *commitError
		if errors.As(err, &ce) {
			// The insert may have committed even though Commit returned an
			// error. The transaction status decides, exactly as for rule
			// transactions; Create reserves no frames, so only the provider
			// slot needs undoing.
			committed = c.resolveCommit(runID, ce, nil, nil)
		}
		if !committed {
			// slot is nil when BeginTx itself failed.
			if slot != nil {
				slot.abandon()
			}
			return "", err
		}
	}

	slot.start(providerCallJob(opProvision, runID, func(ctx context.Context) error {
		_, err := c.provider.Provision(ctx, runID)
		// Carry the actual return moment: a retried completion write must not
		// postpone the T_hello deadline.
		returnedAt := c.clock.Now()
		c.complete(ctx, runID, evProvisionDone{returnedAt: returnedAt, err: err})
		return err
	}))
	return runID, nil
}

// Get implements RunService.
func (c *Controller) Get(ctx context.Context, runID string) (RunDetail, error) {
	return c.store.getRun(ctx, runID)
}

// List implements RunService.
func (c *Controller) List(ctx context.Context) ([]RunSummary, error) {
	return c.store.listRuns(ctx)
}

// End implements RunService. It is idempotent.
func (c *Controller) End(ctx context.Context, runID string) error {
	_, err := c.applyEvent(ctx, runID, evEnd{}, applyOpts{})
	return err
}

// AppendInbox implements RunService.
//
// A subscription_completed event is deduplicated by (run_id, DedupKey): when
// the row already exists, the existing inbox sequence is returned with
// duplicate set and the run is not changed.
func (c *Controller) AppendInbox(ctx context.Context, runID string, ev InboxEvent) (int64, bool, error) {
	if err := validateInboxEvent(ev); err != nil {
		return 0, false, err
	}
	appendEv, err := newAppendEvent(ev)
	if err != nil {
		return 0, false, err
	}
	res, err := c.applyEvent(ctx, runID, appendEv, applyOpts{})
	if err != nil {
		return 0, false, err
	}
	if res.duplicate {
		return res.duplicateSeq, true, nil
	}
	for _, effect := range res.effects {
		if in, ok := effect.(insertInbox); ok {
			return in.seq, false, nil
		}
	}
	return 0, false, errors.New("runcontroller: append produced no inbox sequence")
}

// validateInboxEvent applies the constraints of the AppendInbox interface.
func validateInboxEvent(ev InboxEvent) error {
	switch ev.Kind {
	case KindHumanMessage:
		if len(ev.Text) == 0 || len(ev.Text) > MaxTextBytes {
			return ErrInvalidEvent
		}
	case KindSubscriptionCompleted:
		if ev.EventID == "" || ev.DedupKey == "" || ev.Subscription == nil {
			return ErrInvalidEvent
		}
		return validateSubscriptionCompletion(ev.Subscription)
	default:
		return ErrInvalidEvent
	}
	return nil
}

// validateSubscriptionCompletion applies the same constraints as the
// agentprotocol validation of a subscription_completed inbox message, so an
// event accepted here always passes ValidateControlFrame.
func validateSubscriptionCompletion(sc *SubscriptionCompletion) error {
	for _, value := range []string{
		sc.SubID, sc.System, sc.Event, sc.SubscriptionStatus, sc.Terminal, sc.Source,
	} {
		if value == "" {
			return ErrInvalidEvent
		}
	}
	if sc.OccurredAt.IsZero() || timestamppb.New(sc.OccurredAt).CheckValid() != nil {
		return ErrInvalidEvent
	}
	correlationBytes := 0
	for key, value := range sc.Correlation {
		if key == "" {
			return ErrInvalidEvent
		}
		correlationBytes += len(key) + len(value)
	}
	if correlationBytes > MaxCorrelationBytes {
		return ErrInvalidEvent
	}
	if len(sc.Detail) > MaxDetailBytes {
		return ErrInvalidEvent
	}
	return nil
}

// newAppendEvent converts a validated inbox event into its rule event.
// Human messages carry only text; subscription completions carry the
// identity fields and the JSON-encoded payload stored in inbox.payload.
func newAppendEvent(ev InboxEvent) (evAppend, error) {
	switch ev.Kind {
	case KindHumanMessage:
		return evAppend{kind: KindHumanMessage, text: ev.Text}, nil
	case KindSubscriptionCompleted:
		payload, err := json.Marshal(ev.Subscription)
		if err != nil {
			return evAppend{}, fmt.Errorf("runcontroller: encode subscription completion: %w", err)
		}
		return evAppend{
			kind:     KindSubscriptionCompleted,
			eventID:  ev.EventID,
			dedupKey: ev.DedupKey,
			payload:  payload,
		}, nil
	default:
		return evAppend{}, ErrInvalidEvent
	}
}

func validateText(text string) error {
	if len(text) == 0 || len(text) > MaxTextBytes {
		return ErrInvalidEvent
	}
	return nil
}

// randomID returns n characters drawn uniformly from runIDAlphabet.
func randomID(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = runIDAlphabet[int(b)%len(runIDAlphabet)]
	}
	return string(buf), nil
}
