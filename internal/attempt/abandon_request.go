package attempt

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

var ErrAlreadyAbandoned = errors.New("the original execution was already abandoned")

var ErrAbandonInput = errors.New("original abandonment input identity is unavailable")

// AbandonTx runs only inside the task owner's accounting transaction. Abandoned
// is a durable projection obligation, not a statement that any process exited.
func (s *Service) AbandonTx(tx *ledger.Tx, id, owner string, revision uint64, row task.Attempt, at time.Time, source AbandonContext) (task.RecoveryUsage, error) {
	r, err := GetTx(tx, id)
	if err != nil {
		return task.RecoveryUsage{}, err
	}
	if owner == "" || r.ForceStop == nil || r.ForceStop.Revision != revision {
		return task.RecoveryUsage{}, ErrForceStopChanged
	}
	if r.Abandoned != nil {
		if r.Abandoned.By == owner && r.Abandoned.ForceStopRevision == revision {
			return task.RecoveryUsage{}, ErrAlreadyAbandoned
		}
		return task.RecoveryUsage{}, ErrForceStopChanged
	}
	if r.ForceStop.Level != "exhausted" || !r.Unsettled || !TaskStopOwed(r) {
		return task.RecoveryUsage{}, ErrForceStopChanged
	}
	if row.ExecutionID != r.ID || row.TurnID != r.TurnID || r.Execution == nil || row.ExecutionEpoch != r.Execution.Epoch {
		return task.RecoveryUsage{}, errors.New("abandonment accounting belongs to another execution")
	}
	tracked, err := nativeTaskTx(tx, r)
	if err != nil {
		return task.RecoveryUsage{}, err
	}
	if err := checkIdentityRows(tx); err != nil {
		return task.RecoveryUsage{}, err
	}
	if r.Session != "" {
		latest, err := latestSessionIdentity(tx, sessionIdentityKey(r.Node, r.Harness, r.Session))
		if err != nil {
			return task.RecoveryUsage{}, err
		}
		if latest != r.ID {
			return task.RecoveryUsage{}, errors.New("original native session is now bound to another execution")
		}
	}
	before, err := json.Marshal(r)
	if err != nil {
		return task.RecoveryUsage{}, err
	}
	usage := frozenAccounting(r, row)
	if usage.Reported || usage.Tokens != (task.Tokens{}) {
		frozen := Usage{Model: usage.Model, Input: usage.Tokens.Input, Output: usage.Tokens.Output, CachedRead: usage.Tokens.CachedRead, CachedWrite: usage.Tokens.CachedWrite, Reported: usage.Reported}
		if r.Usage != nil {
			frozen.Context = r.Usage.Context
		}
		r.Usage = &frozen
	}
	if source.State != "absent" && source.State != "original" && source.State != "different" {
		return task.RecoveryUsage{}, errors.New("abandonment requires the original session-slot snapshot")
	}
	input, err := AbandonInputTx(tx, r)
	if err != nil {
		return task.RecoveryUsage{}, err
	}
	recoveryID, err := s.startWorkspaceRecoveryTx(tx, r, source.RecoveryBaseline, at, owner)
	if err != nil {
		return task.RecoveryUsage{}, err
	}
	r.Abandoned = &Abandoned{WorkspaceRecoveryID: recoveryID, SlotState: source.State, SlotFingerprint: source.Fingerprint, ImportFingerprint: source.ImportFingerprint, At: at, By: owner, ForceStopRevision: revision, Reason: r.ForceStop.Reason, Conversation: tracked.Channel, MessageID: input, Session: r.Session}
	if r.EndedAt.IsZero() {
		r.EndedAt = at
	}
	op := ledger.Operation{ID: r.ID, Kind: kind, State: string(r.State), Revision: r.Revision, Data: before}
	r.Revision++
	if err := setRecordDataTx(tx, &op, r); err != nil {
		return task.RecoveryUsage{}, err
	}
	if err := tx.RecordTransition(op, string(r.State), owner); err != nil {
		return task.RecoveryUsage{}, err
	}
	// The episode is written before this AB record in the same task Tx.
	// Validate only after both owner facts are complete; refusal rolls back all.
	if recoveryID != "" {
		if _, err := recoveryByIDTx(tx, recoveryID); err != nil {
			return task.RecoveryUsage{}, err
		}
	}
	return usage, nil
}

// Both sources name this execution. Never discard counters already charged to
// its task when the last persisted native receipt has an older total.
func frozenAccounting(r Record, row task.Attempt) task.RecoveryUsage {
	usage := StoppedUsage(r)
	usage.Tokens.Input = max(usage.Tokens.Input, row.Tokens.Input)
	usage.Tokens.Output = max(usage.Tokens.Output, row.Tokens.Output)
	usage.Tokens.CachedRead = max(usage.Tokens.CachedRead, row.Tokens.CachedRead)
	usage.Tokens.CachedWrite = max(usage.Tokens.CachedWrite, row.Tokens.CachedWrite)
	usage.Tokens.Total = usage.Tokens.Input + usage.Tokens.Output
	usage.Reported = usage.Reported || row.UsageKnown != nil && *row.UsageKnown
	if usage.Model == "" {
		usage.Model = row.Model
	}
	return usage
}
