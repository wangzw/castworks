package runcontroller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/wangzw/castworks/internal/sandboxprovider"
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
func (c *Controller) AppendInbox(ctx context.Context, runID string, text string) (int64, error) {
	if err := validateText(text); err != nil {
		return 0, err
	}
	effects, err := c.applyEvent(ctx, runID, evAppend{text: text}, applyOpts{})
	if err != nil {
		return 0, err
	}
	for _, effect := range effects {
		if in, ok := effect.(insertInbox); ok {
			return in.seq, nil
		}
	}
	return 0, errors.New("runcontroller: append produced no inbox sequence")
}

func validateText(text string) error {
	if len(text) == 0 || len(text) > MaxTextBytes {
		return ErrInvalidText
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
