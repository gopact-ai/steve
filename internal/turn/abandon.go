package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
)

// AbandonControl owns the decision to give up an original execution, distinct
// from physical stop confirmation and from any subsequent workspace cleanup.
type AbandonControl struct{ coordinator *Coordinator }

func NewAbandonControl(c *Coordinator) *AbandonControl { return &AbandonControl{coordinator: c} }

func (a *AbandonControl) AbandonAttempt(ctx context.Context, id, requester string, revision uint64) (attempt.Record, error) {
	return attempt.Record{}, errors.New("execution abandonment is not available")
}
