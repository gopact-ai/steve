package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
)

// A child result can arrive while a sibling is landing into the parent's
// workspace. Keep the original turn and delivery identity while Open is
// definitively refused; no model input or attempt has been committed yet.
// Failures after Open, unknown outcomes, and cancellation are never replayed.
type continuationAttempts struct {
	lifecycle.Attempts
	waiting func()
}

func (a continuationAttempts) Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
	if spec.ID == "" {
		spec.ID = attempt.NewID()
	}
	delay := 250 * time.Millisecond
	notified := false
	for {
		if err := ctx.Err(); err != nil {
			return attempt.Record{}, err
		}
		record, err := a.Attempts.Open(ctx, spec)
		var busy attempt.Busy
		var full attempt.NoSlot
		if err == nil || record.ID != "" || (!errors.As(err, &busy) && !errors.As(err, &full)) {
			return record, err
		}
		if !notified && a.waiting != nil {
			a.waiting()
			notified = true
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempt.Record{}, ctx.Err()
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}
