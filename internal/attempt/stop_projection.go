package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

var errStopAccountingPending = errors.New("task stop accounting is not projected")

// nodeOwnedStop is an execution a durable task stop can reach on its node.
func nodeOwnedStop(r Record) bool {
	return r.State != Superseded && (strings.HasPrefix(r.Session, "ns_") || PendingSessionOpen(r)) && r.Node != "" && r.Execution != nil
}

// TaskStopConfirmed reports that r's own task stop committed with settled
// native evidence. Its accounting may still be unprojected.
func TaskStopConfirmed(r Record) bool {
	return nodeOwnedStop(r) && taskStopAlreadySettled(r) && r.StopEvidence == "task-stop/"+r.ID
}

// StoppedUsage is the usage a confirmed task stop projects onto its task's
// accounting row.
func StoppedUsage(r Record) task.RecoveryUsage {
	if r.Usage == nil {
		return task.RecoveryUsage{}
	}
	u := r.Usage
	return task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}, Model: u.Model, Reported: u.Reported}
}

// StopAccountingSettled reports that row, the accounting row of r's
// execution and turn, is closed and carries the usage r confirmed.
func StopAccountingSettled(r Record, row task.Attempt) bool {
	if row.Open() || row.UsageKnown == nil {
		return false
	}
	u := StoppedUsage(r)
	known := u.Reported || u.Tokens.Input != 0 || u.Tokens.Output != 0 || u.Tokens.CachedRead != 0 || u.Tokens.CachedWrite != 0
	return !known || *row.UsageKnown && row.Tokens == u.Tokens && row.Model == u.Model
}

// MarkStopProjected retires a confirmed task stop from the stop pass once
// its accounting row, read in the same transaction, is settled with the
// record's usage. It reports false, and writes nothing, while the
// accounting is still owed.
func (s *Service) MarkStopProjected(ctx context.Context, id, actor string) (bool, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if current.StopProjected {
		return true, nil
	}
	if !TaskStopConfirmed(current) {
		return false, errors.New("only a confirmed task stop can retire its accounting")
	}
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), actor, nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var next Record
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if next.ID != id || !TaskStopConfirmed(next) || next.StopProjected {
			return errors.New("task stop changed before its accounting was retired")
		}
		row, _, found, err := task.ReadAccountingTx(tx, next.TaskID, next.ID, next.TurnID)
		if err != nil {
			return err
		}
		if !found || !StopAccountingSettled(next, row) {
			return errStopAccountingPending
		}
		next.StopProjected, next.Revision = true, op.Revision+1
		return setRecordDataTx(tx, op, next)
	})
	if errors.Is(err, errStopAccountingPending) {
		return false, nil
	}
	return err == nil, err
}
