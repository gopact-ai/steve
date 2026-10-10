package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// idleTaskAge is how long a chat thread may go unspoken to before its
// task is closed. A person can always start a new one by talking.
const idleTaskAge = 24 * time.Hour

// sweepIdleTasks closes chat tasks that have gone quiet, at start and
// then hourly, and puts each closing in the history. book is the ledger
// tasks are kept in.
func sweepIdleTasks(ctx context.Context, book *ledger.Ledger, tasks *task.Store, attempts *attempt.Service, view *readmodel.Model, reserve func(context.Context, string) (func(), error)) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		closeIdleTasks(ctx, book, tasks, attempts, view, idleTaskAge, reserve)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// closeIdleTasks is one pass of the sweep.
func closeIdleTasks(ctx context.Context, book *ledger.Ledger, tasks *task.Store, attempts *attempt.Service, view *readmodel.Model, age time.Duration, reserve func(context.Context, string) (func(), error)) {
	held := map[string]func(){}
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	live := func(id string) (bool, error) {
		if reserve != nil {
			tracked, exists := tasks.Get(id)
			if !exists {
				return true, nil
			}
			if _, exists := held[tracked.Channel]; !exists {
				release, err := reserve(ctx, tracked.Channel)
				if err != nil {
					return true, nil
				}
				held[tracked.Channel] = release
			}
		}
		_, ok, err := attempts.LiveAttemptOf(ctx, id)
		return ok, err
	}
	// A quiet task still stays open while anything of it is unsettled — a
	// turn waiting on the owner, say — by the ledger checks /complete makes.
	// No input asked for this close, so none is spared, not even a line
	// still queued: it is about to continue the task. Each quiet task is
	// checked and closed in a transaction of its own; the console is read
	// once for the pass, before any of them, and read again in a task's
	// transaction only if it has been written since.
	read := console.ReadCompletion(book)
	settled := func(tx *ledger.Tx, t task.Task) error {
		return turn.CheckTaskCompletionTx(tx, map[string]bool{t.ID: true}, t.Channel, "", false, read.CheckTaskCompletionTx)
	}
	closed, err := tasks.CloseIdle(age, live, settled)
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
