package turn

import (
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

// resolveStoppedExecution projects an already committed physical stop before
// resolving its local observer. Settlement names the original accounting row,
// never the task's current turn or its replacement recovery workspace.
func (c *Coordinator) resolveStoppedExecution(record attempt.Record) error {
	if record.ID == "" || record.Execution == nil || record.Execution.TaskID != record.TaskID ||
		!record.State.Terminal() || record.Unsettled || record.StopEvidence == "" ||
		record.SessionSettled == nil || !*record.SessionSettled {
		return errors.New("execution resolution requires the original identity and committed stop evidence")
	}
	if c.tasks == nil {
		return errors.New("execution resolution requires task accounting")
	}
	tracked, ok := c.tasks.Get(record.TaskID)
	if !ok {
		return errors.New("execution resolution source task is missing")
	}
	for _, row := range tracked.Attempts {
		if row.ExecutionID != record.ID {
			continue
		}
		if row.TurnID != record.TurnID || row.ExecutionEpoch != record.Execution.Epoch {
			return errors.New("execution resolution accounting belongs to another turn or task epoch")
		}
		var usage task.RecoveryUsage
		if u := record.Usage; u != nil {
			usage = task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}, Model: u.Model, Reported: u.Reported}
		}
		outcome := task.OutcomeCancelled
		if record.State == attempt.Superseded {
			outcome = task.OutcomeError
		}
		if err := c.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome, usage); err != nil {
			return fmt.Errorf("attempt %s: stop confirmed; original accounting remains pending: %w", record.ID, err)
		}
		if c.executions != nil {
			c.executions.ResolveStopped(record.ID, *record.Execution)
		}
		return nil
	}
	return fmt.Errorf("attempt %s: execution resolution has no bound accounting row", record.ID)
}
