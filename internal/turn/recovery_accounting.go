package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/task"
)

// ChatObserverActive keeps background accounting/delivery behind this
// generation's driver, including the interval after Bound but before Handle
// has returned. A persisted completion is not proof that its live observer
// has finished delivering it.
func (c *Coordinator) ChatObserverActive(conversation, member string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancels[sessionKey(conversation, member)] != nil
}

// SettleChatAccounting consumes the original execution's durable terminal
// evidence. It neither delivers a reply nor grants a new execution.
func (c *Coordinator) SettleChatAccounting(ctx context.Context, id string) error {
	record, err := c.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	if record.Kind != attempt.KindChat || !record.State.Terminal() || record.Unsettled ||
		record.SessionSettled == nil || !*record.SessionSettled {
		return fmt.Errorf("attempt %s lacks committed chat settlement: %w", id, harness.ErrStopUnconfirmed)
	}
	if record.Execution == nil || record.Execution.TaskID != record.TaskID {
		return fmt.Errorf("attempt %s lacks its original task execution token", id)
	}
	tracked, found := c.tasks.Get(record.TaskID)
	if !found {
		return fmt.Errorf("attempt %s has no original task", id)
	}
	matched := false
	for _, row := range tracked.Attempts {
		if row.ExecutionID != record.ID {
			continue
		}
		if row.TurnID != record.TurnID || row.ExecutionEpoch != record.Execution.Epoch {
			return fmt.Errorf("attempt %s accounting identity changed", id)
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("attempt %s has no bound accounting row", id)
	}
	var runErr error
	if record.State == attempt.Bound {
		var result Result
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &result) != nil {
			return fmt.Errorf("attempt %s has no readable committed chat result", id)
		}
	} else {
		runErr = errors.New(record.Error)
	}
	if err := c.finishRetainedTask(record, runErr, nil); err != nil {
		return err
	}
	if c.executions != nil {
		// A committed prompt settlement and exact accounting supersede the
		// old observer error, never a still-running owner or a newer epoch.
		c.executions.ResolveStopped(record.ID, *record.Execution)
	}
	return nil
}

// finishChatAccounting is called before any continuation callback. A committed
// execution settles its exact row, not whichever task turn happens to be last.
func (c *Coordinator) finishChatAccounting(ctx context.Context, id string, record attempt.Record, turnErr error, tokens task.Tokens, model string) error {
	if id == "" {
		return nil
	}
	if record.ID != "" {
		current, err := c.attempts.Get(ctx, record.ID)
		if err != nil {
			return err
		}
		if current.State == attempt.Bound {
			return c.SettleChatAccounting(ctx, current.ID)
		}
	}
	_, err := c.tasks.FinishAs(id, lifecycle.OutcomeOf(turnErr), tokens, 0, model)
	return err
}

func (c *Coordinator) notifyAccountedTurn(taskID string) {
	if c.afterTurn != nil && taskID != "" {
		c.afterTurn(taskID)
	}
}
