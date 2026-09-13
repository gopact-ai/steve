package turn

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

func (c commands) taskComplete(ctx context.Context, title string, tracked task.Task) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	complete := func() error {
		current, ok := c.tasks.Get(tracked.ID)
		if !ok || current.Channel != tracked.Channel {
			return task.ErrCompleteRoot
		}
		if !current.CompletedByUser && c.cancels[sessionKey(current.Channel, current.Member)] != nil {
			return task.ErrCompleteBusy
		}
		if c.plans != nil {
			if _, ok := c.plans.ForTask(current.ID); ok {
				return task.ErrCompleteRoot
			}
		}
		var guard func(*ledger.Tx, map[string]bool) error
		if c.attempts != nil {
			guard = func(tx *ledger.Tx, ids map[string]bool) error {
				return c.checkTaskCompletionTx(tx, ids, current.Channel)
			}
		}
		_, err := c.tasks.CompleteRoot(ctx, current.ID, current.Channel, guard)
		return err
	}
	var err error
	if c.executions != nil && !tracked.CompletedByUser {
		err = c.executions.WhileTaskIdle(tracked.ID, complete)
	} else {
		err = complete()
	}
	if err != nil {
		key := i18n.TaskCompleteFailed
		switch {
		case errors.Is(err, task.ErrCompleteRoot):
			key = i18n.TaskCompleteRoot
		case errors.Is(err, task.ErrCompleteState):
			key = i18n.TaskCompleteState
		case errors.Is(err, task.ErrCompleteBusy):
			key = i18n.TaskCompleteBusy
		case errors.Is(err, task.ErrCompleteChildren):
			key = i18n.TaskCompleteChildren
		case errors.Is(err, task.ErrCompleteDelivery):
			key = i18n.TaskCompleteDelivery
		case errors.Is(err, task.ErrCompleteAttention):
			key = i18n.TaskCompleteAttention
		}
		text := c.text.T(key, tracked.ID)
		return Result{Title: title, Text: text}, UserError{Text: text}
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskCompleted, tracked.ID)}, nil
}

func (c commands) checkTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation string) error {
	if err := attempt.CheckTaskCompletionTx(tx, ids); err != nil {
		return err
	}
	var operations []ledger.Operation
	for _, kind := range []string{"landing", "intent", "disclosure-request"} {
		current, err := tx.Operations(kind, "")
		if err != nil {
			return err
		}
		operations = append(operations, current...)
	}
	for _, operation := range operations {
		switch operation.Kind {
		case "landing":
			var landing artifact.Landing
			if err := json.Unmarshal(operation.Data, &landing); err != nil {
				return err
			}
			if landing.Source != nil && landing.Source.Execution != nil && ids[landing.Source.Execution.TaskID] && operation.State != artifact.LandCommitted {
				return task.ErrCompleteDelivery
			}
		case "intent":
			var effect intent.Intent
			if err := json.Unmarshal(operation.Data, &effect); err != nil {
				return err
			}
			if ids[effect.TaskID] && operation.State != string(intent.Succeeded) && operation.State != string(intent.Failed) {
				return task.ErrCompleteAttention
			}
		case "disclosure-request":
			var disclosure project.DisclosureRequest
			if err := json.Unmarshal(operation.Data, &disclosure); err != nil {
				return err
			}
			if ids[disclosure.TaskID] && operation.State == project.DisclosureProposed {
				return task.ErrCompleteAttention
			}
		}
	}
	var plans struct {
		ByTask map[string]string `json:"by_task"`
	}
	if raw, _, err := tx.LoadDocument("plans"); err != nil {
		return err
	} else if len(raw) != 0 {
		if err := json.Unmarshal(raw, &plans); err != nil {
			return err
		}
		for id := range ids {
			if plans.ByTask[id] != "" {
				return task.ErrCompleteRoot
			}
		}
	}
	var console struct {
		Questions map[string]consoleapi.PendingQuestion `json:"questions"`
		Exchanges map[string][]consoleapi.Exchange      `json:"exchanges"`
	}
	if raw, _, err := tx.LoadDocument("console"); err != nil {
		return err
	} else if len(raw) != 0 {
		if err := json.Unmarshal(raw, &console); err != nil {
			return err
		}
		for _, question := range console.Questions {
			if question.State == "pending" && (ids[question.TaskID] || question.Conversation == conversation) {
				return task.ErrCompleteAttention
			}
		}
		for _, exchanges := range console.Exchanges {
			for _, exchange := range exchanges {
				if ids[exchange.ExpectedTask] && !exchange.State.Terminal() {
					return task.ErrCompleteDelivery
				}
				if exchange.Conversation == conversation && (exchange.State == consoleapi.ExchangeAwaitingUser || exchange.State == consoleapi.ExchangeRecovering) {
					return task.ErrCompleteAttention
				}
				if exchange.Conversation == conversation && !exchange.State.Terminal() {
					_, parsed := c.ParseInput(exchange.Input)
					verb, _, valid := parseTaskArgs(parsed.Rest)
					if parsed.Command != protocol.CommandTasks || !valid || verb != taskComplete {
						return task.ErrCompleteDelivery
					}
				}
			}
		}
	}
	return nil
}
