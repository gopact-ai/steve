package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
)

func ExportProject(ctx context.Context, book *ledger.Ledger, project string) (ledger.TransferFacts, error) {
	ops, err := book.Operations(ctx, runKind, "")
	if err != nil {
		return ledger.TransferFacts{}, err
	}
	var ids []string
	for _, op := range ops {
		var run RunRecord
		if err := json.Unmarshal(op.Data, &run); err != nil {
			return ledger.TransferFacts{}, err
		}
		if run.ProjectID == project {
			ids = append(ids, op.ID)
		}
	}
	facts, err := book.ExportOperations(ctx, ids)
	if err != nil {
		return facts, err
	}
	if raw, err := book.Bindings(ctx, runKind); err != nil {
		return facts, err
	} else {
		for _, data := range raw {
			var run RunRecord
			if json.Unmarshal(data, &run) != nil {
				return facts, fmt.Errorf("invalid legacy run")
			}
			if run.ProjectID == project {
				return facts, fmt.Errorf("project %s has unmigrated legacy plan-run state", project)
			}
		}
	}
	return facts, nil
}

func RemapTransfer(f *ledger.TransferFacts, m ledger.TransferIDs, target project.Home) error {
	for i := range f.Operations {
		op := &f.Operations[i]
		var run RunRecord
		if err := json.Unmarshal(op.Data, &run); err != nil {
			return err
		}
		run.ID = m.Operation(run.ID)
		run.PlanID = m.Plan(run.PlanID)
		run.TaskID = m.Task(run.TaskID)
		if run.Execution != nil {
			run.Execution.TaskID = m.Task(run.Execution.TaskID)
		}
		run.RunID = runIDFor(plan.Plan{ID: run.PlanID, Rev: run.Rev})
		run.Target = target
		for j := range run.Sinks {
			run.Sinks[j].LandingID = m.Operation(run.Sinks[j].LandingID)
			if run.Sinks[j].Source != nil && run.Sinks[j].Source.Execution != nil {
				run.Sinks[j].Source.Execution.TaskID = m.Task(run.Sinks[j].Source.Execution.TaskID)
			}
		}
		op.Data, _ = json.Marshal(run)
	}
	f.RemapEnvelopes(m)
	return nil
}

// RemapAttemptOutputs owns the versioned plan output carried by attempts.
func RemapAttemptOutputs(f *ledger.TransferFacts, m ledger.TransferIDs) error {
	for i := range f.Operations {
		op := &f.Operations[i]
		var record attempt.Record
		if err := json.Unmarshal(op.Data, &record); err != nil {
			return err
		}
		if record.Kind != attempt.KindStep || record.Result == nil || len(record.Result.Output) == 0 {
			continue
		}
		var out stepOutput
		if err := json.Unmarshal(record.Result.Output, &out); err != nil {
			return err
		}
		out.PlanID = m.Plan(out.PlanID)
		plan.RemapResult(&out.Result, m)
		record.Result.Output, _ = json.Marshal(out)
		op.Data, _ = json.Marshal(record)
	}
	return nil
}

// FreezeProjectRunsTx ends source orchestration responsibility after the
// original run was exported. It does not claim execution completed its goal.
func FreezeProjectRunsTx(tx *ledger.Tx, id string) error {
	ops, err := tx.Operations(runKind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		var r RunRecord
		if err := json.Unmarshal(op.Data, &r); err != nil {
			return err
		}
		if r.ProjectID != id || r.Phase == RunCompleted {
			continue
		}
		r.Phase = RunCompleted
		r.Outcome = "moved"
		r.Error = "project migrated from this hub"
		if err := tx.SetData(&op, r); err != nil {
			return err
		}
		if err := tx.RecordTransition(op, RunCompleted, "project-migration"); err != nil {
			return err
		}
	}
	return nil
}
