package task

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BindAttempt fixes the accounting row to its admitted execution before any
// native input. A delayed receipt can then settle its original row even when
// the task has been resumed and a later turn is already active.
func (s *Store) BindAttempt(token ExecutionToken, attemptID, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return err
	}
	if attemptID == "" {
		return errors.New("execution attempt identity is required")
	}
	next := s.draft()
	tracked := next.edit(token.TaskID)
	row := tracked.primaryAttempt()
	if row == nil {
		return errors.New("task has no execution accounting row")
	}
	// A row keeps the epoch it was opened under: a token of a later epoch
	// cannot adopt one a stop has revoked.
	if !row.Open() || row.ExecutionEpoch != token.Epoch || row.ExecutionID != "" && (row.ExecutionID != attemptID || row.TurnID != turnID) {
		return errors.New("task accounting row belongs to another execution")
	}
	row.ExecutionID, row.TurnID, row.ExecutionEpoch = attemptID, turnID, token.Epoch
	return s.replaceLocked(next)
}

func usageKnown(usage RecoveryUsage) bool {
	return usage.Reported || usage.Tokens.Input > 0 || usage.Tokens.Output > 0 || usage.Tokens.CachedRead > 0 || usage.Tokens.CachedWrite > 0
}

// settleAccounting projects an observed execution total, charging only the
// delta beyond an already recorded total. It never changes task state.
func settleAccounting(next *draft, tracked *Task, row *Attempt, endedAt time.Time, outcome Outcome, usage RecoveryUsage) error {
	usage.Tokens.Total = usage.Tokens.Input + usage.Tokens.Output
	lineage, err := next.lineage(tracked.ID)
	if err != nil {
		return err
	}
	known := usageKnown(usage)
	if known && (usage.Tokens.Input < row.Tokens.Input || usage.Tokens.Output < row.Tokens.Output || usage.Tokens.CachedRead < row.Tokens.CachedRead || usage.Tokens.CachedWrite < row.Tokens.CachedWrite) {
		return errors.New("execution usage receipt regressed its recorded counters")
	}
	delta := Tokens{}
	if known {
		delta = Tokens{Input: usage.Tokens.Input - row.Tokens.Input, Output: usage.Tokens.Output - row.Tokens.Output, CachedRead: usage.Tokens.CachedRead - row.Tokens.CachedRead, CachedWrite: usage.Tokens.CachedWrite - row.Tokens.CachedWrite, Total: usage.Tokens.Total - row.Tokens.Total}
		row.Tokens = usage.Tokens
		row.Model = usage.Model
	}
	if row.UsageKnown == nil || known {
		row.UsageKnown = &known
	}
	var elapsed time.Duration
	if row.Open() {
		row.EndedAt, row.Outcome = endedAt, outcome
		elapsed = max(0, endedAt.Sub(row.StartedAt))
	}
	for _, ancestor := range lineage {
		ancestor.Budget.Elapsed += elapsed
		ancestor.Budget.Tokens = ancestor.Budget.Tokens.Add(delta)
		if endedAt.After(ancestor.UpdatedAt) {
			ancestor.UpdatedAt = endedAt
		}
	}
	return nil
}

// SettleAttempt consumes durable results or stop receipts for exactly one
// execution; it remains valid after that execution's task token was revoked.
func (s *Store) SettleAttempt(ctx context.Context, taskID, attemptID, turnID string, endedAt time.Time, outcome Outcome, usage RecoveryUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if attemptID == "" {
		return errors.New("execution attempt identity is required")
	}
	next := s.draft()
	tracked := next.edit(taskID)
	if tracked == nil {
		return fmt.Errorf("task %s not found", taskID)
	}
	for i := range tracked.Attempts {
		row := &tracked.Attempts[i]
		if row.ExecutionID != attemptID {
			continue
		}
		if row.TurnID != turnID {
			return errors.New("execution receipt belongs to another turn")
		}
		if endedAt.IsZero() {
			endedAt = s.now()
		}
		if err := settleAccounting(next, tracked, row, endedAt, outcome, usage); err != nil {
			return err
		}
		return s.replaceRecordsLocked(ctx, next, nil)
	}
	return errors.New("execution has no bound task accounting row")
}
