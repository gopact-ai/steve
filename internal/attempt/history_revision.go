package attempt

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

const historyRevisionKind = "attempt-history-revision"

func guardRecordOpenTx(tx *ledger.Tx, spec Spec) error {
	if err := task.CheckExecutionTx(tx, spec.Execution); err != nil {
		return err
	}
	if spec.Kind == KindChat {
		if err := task.CheckChatAdmissionTx(tx, spec.TaskID, spec.TurnID, spec.Execution); err != nil {
			return err
		}
	}
	return touchHistoryRevisionTx(tx, spec.TaskID)
}

// A revision is a read-generation token, never execution authority. Keeping it
// in the Record's transaction makes rejected writes invisible to pagination.
// A fresh nonce, rather than a counter, also avoids ABA after snapshot restore.
func historyRevisionTx(tx ledger.Reader, taskID string) (string, error) {
	token, found, err := storedHistoryRevisionTx(tx, taskID)
	if err != nil || found {
		return token, err
	}
	var id string
	err = tx.QueryRow(`SELECT id FROM operations INDEXED BY operations_task_history WHERE kind='attempt' AND `+historyTaskExpression+`=? LIMIT 1`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err == nil {
		return "", fmt.Errorf("attempt: task %q has history but no read revision", taskID)
	}
	return "", err
}

func storedHistoryRevisionTx(tx ledger.Reader, taskID string) (string, bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, historyRevisionKind, taskID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var token string
	if err := json.Unmarshal([]byte(raw), &token); err != nil {
		return "", false, fmt.Errorf("attempt: task %q invalid history revision: %w", taskID, err)
	}
	value, err := hex.DecodeString(token)
	if err != nil || len(value) != 32 || hex.EncodeToString(value) != token {
		return "", false, fmt.Errorf("attempt: task %q invalid history revision", taskID)
	}
	return token, true, nil
}

func writeHistoryRevisionTx(tx *ledger.Tx, taskID string) error {
	var nonce [32]byte
	rand.Read(nonce[:])
	return tx.PutBinding(historyRevisionKind, taskID, hex.EncodeToString(nonce[:]))
}

// The only implicit initialization is before the first Record in a scope is
// created. Existing history without a revision is corrupt, not legacy input.
func touchHistoryRevisionTx(tx *ledger.Tx, taskID string) error {
	if _, err := historyRevisionTx(tx, taskID); err != nil {
		return err
	}
	return writeHistoryRevisionTx(tx, taskID)
}

// setRecordDataTx is the owner save boundary for every Transition, including
// same-state session, quarantine and settlement updates. Both old and new
// scopes are invalidated if an owner mutation changes the task identity.
// It is also where a stop projection mark is dropped once it no longer
// describes the record: see stopProjectionHolds.
func setRecordDataTx(tx *ledger.Tx, op *ledger.Operation, next Record) error {
	previous, err := decodeHistoryRecord(*op)
	if err != nil {
		return err
	}
	if next.StopProjected && !stopProjectionHolds(previous, next) {
		next.StopProjected = false
	}
	if err := tx.SetData(op, next); err != nil {
		return err
	}
	if err := touchHistoryRevisionTx(tx, previous.TaskID); err != nil {
		return err
	}
	if next.TaskID != previous.TaskID {
		return touchHistoryRevisionTx(tx, next.TaskID)
	}
	return nil
}

// ImportHistoryTx is composed with ledger.ImportFacts, in its owner callback.
// Transfer is an explicit bulk initialization boundary: source read tokens
// are not destination authority. An import refreshes only its affected scopes;
// restoring a replica instead preserves the exact tokens in its bindings.
// Imported records keep the source's StopProjected marks. That is sound
// only because the tasks' accounting rows the marks were checked against
// are imported in the same transfer by task.ImportProjectTx; importing
// attempt history without them would retire stops with no settled
// accounting at the destination.
func ImportHistoryTx(tx *ledger.Tx, operations []ledger.Operation, validateOnly bool) error {
	scopes := map[string][]string{}
	for _, op := range operations {
		if op.Kind != kind {
			continue
		}
		record, err := decodeHistoryRecord(op)
		if err != nil {
			return err
		}
		scopes[record.TaskID] = append(scopes[record.TaskID], record.ID)
	}
	ids := make([]string, 0, len(scopes))
	for id := range scopes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if validateOnly {
			// Called before the import writes. Refuse to legitimize an
			// existing scope whose read revision has been lost.
			if _, err := historyRevisionTx(tx, id); err != nil {
				return err
			}
			continue
		}
		_, found, err := storedHistoryRevisionTx(tx, id)
		if err != nil {
			return err
		}
		if !found {
			// Apply must recheck after ValidateImport's transaction. Only
			// records explicitly supplied by this import may initialize a
			// missing scope. The bounded subquery reads at most batch+1 IDs,
			// so a corrupt destination cannot turn this into a history scan.
			expected := make(map[string]bool, len(scopes[id]))
			for _, operationID := range scopes[id] {
				expected[operationID] = true
			}
			var raw string
			if err := tx.QueryRow(`SELECT json_group_array(id) FROM (SELECT id FROM operations INDEXED BY operations_task_history WHERE kind='attempt' AND `+
				historyTaskExpression+`=? LIMIT ?)`, id, len(expected)+1).Scan(&raw); err != nil {
				return err
			}
			var actual []string
			if err := json.Unmarshal([]byte(raw), &actual); err != nil {
				return err
			}
			for _, operationID := range actual {
				if !expected[operationID] {
					return fmt.Errorf("attempt: task %q has unimported history without a read revision", id)
				}
			}
		}
		if err := writeHistoryRevisionTx(tx, id); err != nil {
			return err
		}
	}
	return nil
}
