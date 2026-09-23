package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
)

// waitingAttempts waits out an Open refused for a reason that passes by
// itself, keeping the original turn and delivery identity: while Open is
// definitively refused no model input or attempt has been committed yet.
// Failures after Open, unknown outcomes, and cancellation are never
// replayed.
type waitingAttempts struct {
	lifecycle.Attempts
	// passes says a refusal is worth waiting out.
	passes  func(error) bool
	waiting func()
}

// A child result can arrive while a sibling is landing into the parent's
// workspace, or while every slot of its endpoint is taken: the parent's
// continuation waits for either.
func continuationPasses(err error) bool {
	var busy attempt.Busy
	var full attempt.NoSlot
	return errors.As(err, &busy) || errors.As(err, &full)
}

// Any turn waits out a canonical lock held only to cut a snapshot: that
// holder writes nothing and gives the lock back once the cut is done.
func snapshotPasses(err error) bool {
	var busy attempt.Busy
	return errors.As(err, &busy) && artifact.Snapshotting(busy.Holder)
}

func (a waitingAttempts) Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
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
		if err == nil || record.ID != "" || !a.passes(err) {
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
