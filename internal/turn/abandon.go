package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// AbandonControl owns the decision to give up an original execution, distinct
// from physical stop confirmation and from any subsequent workspace cleanup.
type AbandonControl struct{ coordinator *Coordinator }

func NewAbandonControl(c *Coordinator) *AbandonControl { return &AbandonControl{coordinator: c} }

func (a *AbandonControl) AbandonAttempt(ctx context.Context, id, requester string, revision uint64) (attempt.Record, error) {
	c := a.coordinator
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	owner, err := c.owners.of("console")
	if err != nil {
		return attempt.Record{}, err
	}
	if owner == "" || requester != owner {
		return attempt.Record{}, errors.New("abandonment requires the owner")
	}
	if c.maintaining {
		return attempt.Record{}, errors.New("abandonment is unavailable during maintenance")
	}
	original, err := c.attempts.Get(ctx, id)
	if err != nil {
		return attempt.Record{}, err
	}
	err = c.tasks.AbandonExecution(ctx, original.TaskID, original.ID, original.TurnID, func(tx *ledger.Tx, row task.Attempt, at time.Time) (task.RecoveryUsage, error) {
		source, err := abandonContextTx(tx, original.ID)
		if err != nil {
			return task.RecoveryUsage{}, err
		}
		return c.attempts.AbandonTx(tx, original.ID, requester, revision, row, at, source)
	})
	if errors.Is(err, attempt.ErrForceStopChanged) {
		return attempt.Record{}, UserError{Text: c.text.T(i18n.ConsoleAbandonChanged), Cause: err}
	}
	if err != nil && !errors.Is(err, attempt.ErrAlreadyAbandoned) {
		return attempt.Record{}, err
	}
	return c.attempts.Get(ctx, id)
}
