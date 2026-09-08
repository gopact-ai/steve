package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

// idleTaskAge is how long a chat thread may go unspoken to before its
// task is closed. A person can always start a new one by talking.
const idleTaskAge = 24 * time.Hour

// sweepIdleTasks closes chat tasks that have gone quiet, at start and
// then hourly, and puts each closing in the history.
func sweepIdleTasks(ctx context.Context, tasks *task.Store, attempts *attempt.Service, view *readmodel.Model) {
	live := func(id string) bool {
		_, ok := attempts.LiveAttemptOf(ctx, id)
		return ok
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		for _, t := range tasks.CloseIdle(idleTaskAge, live) {
			slog.Info(fmt.Sprintf("steve: task #%s closed after %s without a word", t.ID, idleTaskAge), "task", t.ID)
			if view != nil {
				view.Observe("task.idle", t.ID, fmt.Sprintf("task #%s (%s) closed: quiet for more than %s", t.ID, t.Member, idleTaskAge))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepAttempts keeps expiring attempts whose drivers stopped renewing.
func sweepAttempts(ctx context.Context, attempts *attempt.Service) {
	ticker := time.NewTicker(attempts.TTL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, err := attempts.Sweep(ctx)
			if err != nil {
				slog.Error(fmt.Sprintf("steve: sweep attempts: %v", err))
			}
			for _, r := range expired {
				slog.Warn(fmt.Sprintf("steve: expired attempt %s", attempt.Describe(r)), "attempt", r.ID, "task", r.TaskID, "node", r.Node)
			}
		}
	}
}
