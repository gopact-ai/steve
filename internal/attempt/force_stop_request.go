package attempt

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ErrForceStopAlreadyStopped makes an exactly proved exit an atomic no-op.
var ErrForceStopAlreadyStopped = errors.New("original process stop is already confirmed")

// RequestForceStopTreeTx is composed with the task owner's atomic cancellation.
// The caller revokes precisely taskIDs in this transaction. Recording the exit
// requirement before those revocations become visible fences ordinary command
// receipts even when an earlier cancellation is already in flight.
func (s *Service) RequestForceStopTreeTx(tx *ledger.Tx, target, taskID, actor string, taskIDs []string, expectedRevision uint64) error {
	if actor == "" || !slices.Contains(taskIDs, taskID) {
		return errors.New("force stop requires its owner and task tree")
	}
	original, err := GetTx(tx, target)
	if err != nil {
		return err
	}
	if original.Abandoned != nil {
		return ErrForceStopChanged
	}
	currentRevision := uint64(0)
	if original.ForceStop != nil {
		currentRevision = original.ForceStop.Revision
	}
	if currentRevision != expectedRevision || original.ForceStop != nil && forceStopActive(original.ForceStop.Level) {
		return ErrForceStopChanged
	}
	if original.TaskID != taskID || !nodeOwnedStop(original) {
		return errors.New("force stop requires the original node-owned execution")
	}
	if err := checkIdentityRows(tx); err != nil {
		return err
	}
	stopped, err := processExitRecordedTx(tx, original)
	if err != nil {
		return err
	}
	if stopped {
		return ErrForceStopAlreadyStopped
	}
	if original.Session != "" {
		latest, err := latestSessionIdentity(tx, sessionIdentityKey(original.Node, original.Harness, original.Session))
		if err != nil {
			return err
		}
		if latest != original.ID {
			return errors.New("original native session is now bound to another execution")
		}
	}
	raw, err := json.Marshal(taskIDs)
	if err != nil {
		return err
	}
	rows, err := tx.Query(tasksByUpdateSQL, string(raw))
	if err != nil {
		return err
	}
	var records []Record
	for rows.Next() {
		r, err := scanIdentityRecord(rows)
		if err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Abandoned != nil || r.ID != target && (!TaskStopOwed(r) || TaskStopConfirmed(r)) {
			continue
		}
		stopped, err := processExitRecordedTx(tx, r)
		if err != nil {
			return err
		}
		if stopped {
			continue
		}
		before, err := json.Marshal(r)
		if err != nil {
			return err
		}
		op := ledger.Operation{ID: r.ID, Kind: "attempt", State: string(r.State), Revision: r.Revision, Data: before}
		r.ForceStop = newForceStop(r.ForceStop, actor, s.now().UTC())
		r.Unsettled = true
		r.Revision++
		if err := setRecordDataTx(tx, &op, r); err != nil {
			return err
		}
		if err := tx.RecordTransition(op, string(r.State), actor); err != nil {
			return err
		}
	}
	return nil
}

// A previously settled command does not prove the process exited. Only an
// already-validated, exactly bound native process receipt makes a retry a no-op.
func processExitRecordedTx(tx *ledger.Tx, r Record) (bool, error) {
	if !taskStopAlreadySettled(r) || r.StopEvidence != "task-stop/"+r.ID && r.StopEvidence != "process-stop/"+r.ID {
		return false, nil
	}
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, taskStopReceiptKind, r.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt TaskStopReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return false, err
	}
	tracked, err := nativeTaskTx(tx, r)
	if err != nil {
		return false, err
	}
	return receipt.AttemptID == r.ID && receipt.Evidence.Session.ProcessStopped && matchesStoppedSession(r, tracked, receipt.Evidence.Session), nil
}
