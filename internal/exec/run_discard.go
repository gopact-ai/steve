package exec

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
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
	byPlan, err := plan.RetirementPlansTx(tx, taskIDs)
	if err != nil {
		return nil, err
	}
	planIDs := make([]string, 0)
	runIDs := make([]string, 0)
	for id, p := range byPlan {
		if selected[p.TaskID] {
			planIDs = append(planIDs, id)
			runIDs = append(runIDs, planRunID(id))
		}
	}
	planRaw, err := json.Marshal(planIDs)
	if err != nil {
		return nil, err
	}
	runRaw, err := json.Marshal(runIDs)
	if err != nil {
		return nil, err
	}
	var runs []retiringRun
	after := ""
	for {
		op := ledger.Operation{Kind: runKind}
		var raw string
		// Select all possible ownership anchors before validating consistency.
		// Missing/null execution carries no token; unreadable token ownership
		// cannot establish that an otherwise unrelated run belongs elsewhere.
		err := tx.QueryRow(`SELECT id,state,revision,data FROM operations WHERE kind=? AND id>? AND (
			json_type(data,'$.task_id') IS NOT 'text'
			OR json_extract(data,'$.task_id') IN (SELECT value FROM json_each(?))
			OR (json_type(data,'$.execution') IS NOT NULL AND json_type(data,'$.execution') IS NOT 'null' AND (
				json_type(data,'$.execution') IS NOT 'object'
				OR json_type(data,'$.execution.task_id') IS NOT 'text'
				OR json_extract(data,'$.execution.task_id')=''
				OR json_extract(data,'$.execution.task_id') IN (SELECT value FROM json_each(?))))
			OR EXISTS(SELECT 1 FROM json_each(data,'$.sinks') AS sink WHERE
				json_type(sink.value,'$.source') IS NOT NULL AND json_type(sink.value,'$.source') IS NOT 'null' AND (
					json_type(sink.value,'$.source.execution.task_id') IS NOT 'text'
					OR json_extract(sink.value,'$.source.execution.task_id')=''
					OR json_extract(sink.value,'$.source.execution.task_id') IN (SELECT value FROM json_each(?))))
			OR json_extract(data,'$.plan_id') IN (SELECT value FROM json_each(?))
			OR id IN (SELECT value FROM json_each(?))) ORDER BY id LIMIT 1`, runKind, after, string(ids), string(ids), string(ids), string(planRaw), string(runRaw)).Scan(&op.ID, &op.State, &op.Revision, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return runs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read retiring plan runs: %w", err)
		}
		op.Data = json.RawMessage(raw)
		rec, err := decodeRun(op)
		if err != nil {
			return nil, err
		}
		if !selected[rec.TaskID] {
			return nil, fmt.Errorf("invalid retiring plan run ownership %s", op.ID)
		}
		if p, found := byPlan[rec.PlanID]; found {
			if err := rec.checkPlan(p); err != nil {
				return nil, err
			}
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
