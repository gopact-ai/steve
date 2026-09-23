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
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		closeIdleTasks(ctx, tasks, attempts, view, idleTaskAge)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// closeIdleTasks is one pass of the sweep.
func closeIdleTasks(ctx context.Context, tasks *task.Store, attempts *attempt.Service, view *readmodel.Model, age time.Duration) {
	live := func(id string) (bool, error) {
		_, ok, err := attempts.LiveAttemptOf(ctx, id)
		return ok, err
	}
	closed, err := tasks.CloseIdle(age, live)
	if err != nil && ctx.Err() == nil {
		// Each named task stays open; the next pass looks at it again.
		slog.Warn(fmt.Sprintf("steve: idle sweep left tasks open: %v", err))
	}
	for _, t := range closed {
		slog.Info(fmt.Sprintf("steve: task #%s closed after %s without a word", t.ID, age), "task", t.ID)
		if view != nil {
			// Keys: task, member, idle.
			view.Observe("task.idle", t.ID, fmt.Sprintf("task #%s (%s) closed: quiet for more than %s", t.ID, t.Member, age),
				map[string]string{"task": t.ID, "member": t.Member, "idle": age.String()})
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
