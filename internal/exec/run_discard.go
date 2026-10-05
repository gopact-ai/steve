package exec

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

type retiringRun struct {
	operation ledger.Operation
	record    RunRecord
}

// CheckTaskRunRetirementTx refuses deletion while a plan driver still owns
// an unfinished run. Expiry alone is not proof the driver stopped; recovery
// invalidates its lease, or the driver releases it after joining. A completed
// run owes no orchestration even if a crash interrupted its lease release.
func CheckTaskRunRetirementTx(tx ledger.Reader, taskIDs []string) error {
	_, err := taskRunsForDiscardTx(tx, taskIDs)
	return err
}

func taskRunsForDiscardTx(tx ledger.Reader, taskIDs []string) ([]retiringRun, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	if err := ledger.CheckOperationEnvelopesTx(tx, runKind); err != nil {
		return nil, err
	}
	ids, err := json.Marshal(taskIDs)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		selected[id] = true
	}
	var runs []retiringRun
	after := ""
	for {
		op := ledger.Operation{Kind: runKind}
		var raw string
		// Unreadable task ownership cannot prove a run belongs elsewhere.
		err := tx.QueryRow(`SELECT id,state,revision,data FROM operations WHERE kind=? AND id>? AND (json_type(data,'$.task_id') IS NOT 'text' OR json_extract(data,'$.task_id') IN (SELECT value FROM json_each(?))) ORDER BY id LIMIT 1`, runKind, after, string(ids)).Scan(&op.ID, &op.State, &op.Revision, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return runs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read retiring plan runs: %w", err)
		}
		op.Data = json.RawMessage(raw)
		var rec RunRecord
		if err := json.Unmarshal(op.Data, &rec); err != nil {
			return nil, fmt.Errorf("read plan run %s: %w", op.ID, err)
		}
		if !selected[rec.TaskID] || rec.PlanID == "" || rec.ID != op.ID || rec.ID != planRunID(rec.PlanID) || rec.Phase != op.State || rec.Execution != nil && rec.Execution.TaskID != rec.TaskID {
			return nil, fmt.Errorf("invalid retiring plan run %s", op.ID)
		}
		switch op.State {
		case RunExecuting, RunLanding, RunCompleted:
		default:
			return nil, fmt.Errorf("plan run %s has unknown phase %s", op.ID, op.State)
		}
		if op.State != RunCompleted {
			var held int
			if err := tx.QueryRow(`SELECT count(*) FROM leases WHERE resource_key=? AND holder!=''`, "plan-driver:"+rec.ID).Scan(&held); err != nil {
				return nil, err
			}
			if held != 0 {
				return nil, fmt.Errorf("%w: plan run %s still has a driver", task.ErrRetirementPending, rec.ID)
			}
		}
		runs = append(runs, retiringRun{operation: op, record: rec})
		after = op.ID
	}
}

// DiscardTaskRunsTx ends orchestration responsibility with conversation task
// deletion, after the caller's native-retirement guards pass. It never removes
// plan history, clears stop obligations, or claims a discarded goal succeeded.
func DiscardTaskRunsTx(tx *ledger.Tx, taskIDs []string) error {
	runs, err := taskRunsForDiscardTx(tx, taskIDs)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.operation.State == RunCompleted {
			continue
		}
		rec := run.record
		rec.Phase, rec.Outcome = RunCompleted, "discarded"
		if err := tx.SetData(&run.operation, rec); err != nil {
			return err
		}
		if err := tx.RecordTransition(run.operation, RunCompleted, "conversation-discard"); err != nil {
			return err
		}
	}
	return nil
}
