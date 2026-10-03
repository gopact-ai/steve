package task

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// SetPendingTurnWorkspace updates only a not-yet-bound turn at a safe recovery
// admission boundary. It cannot relocate a live execution or native context.
func (s *Store) SetPendingTurnWorkspace(ctx context.Context, token ExecutionToken, turnID, workspace string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.draft()
	stored := next.edit(token.TaskID)
	if stored == nil || turnID == "" || workspace == "" {
		return ErrExecutionStopped
	}
	if err := checkExecutionBy(next.find, token); err != nil {
		return err
	}
	row := stored.primaryAttempt()
	if row == nil || !row.Open() || row.TurnID != turnID || row.ExecutionID != "" || row.ExecutionEpoch != token.Epoch {
		return errors.New("workspace refresh requires the exact unadmitted turn")
	}
	stored.Workspace = workspace
	return s.replaceRecordsLocked(ctx, next, func(tx *ledger.Tx) error { return CheckExecutionTx(tx, &token) })
}
