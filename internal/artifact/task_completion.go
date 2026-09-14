package artifact

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type completionLandingKey struct {
	Project, Artifact, Attempt string
	Target                     project.Home
	Execution                  task.ExecutionToken
}

func completionLandingIdentity(land Landing) completionLandingKey {
	return completionLandingKey{Project: land.Project, Artifact: land.Artifact, Attempt: land.Source.AttemptID, Target: land.Target, Execution: *land.Source.Execution}
}

// CheckTaskLandingsTx retains real conflicts and unfinished WALs. Older queue
// drainers could race, leaving a terminal lock refusal alongside a successful
// landing of the exact same result. That refusal never acquired a write lease;
// its historical record remains intact without blocking accepted work forever.
// Unapplied is explicit preapply-stop proof; snapshots and candidate merge paths
// remain evidence even when no canonical write was admitted.
func CheckTaskLandingsTx(tx *ledger.Tx, ids map[string]bool) error {
	queue, err := tx.Bindings(pendingKind)
	if err != nil {
		return err
	}
	for _, raw := range queue {
		var pending Pending
		if err := json.Unmarshal(raw, &pending); err != nil {
			return err
		}
		if pending.Source != nil && pending.Source.Execution != nil && ids[pending.Source.Execution.TaskID] {
			return fmt.Errorf("%w: artifact %s is queued to land", task.ErrCompleteDelivery, pending.Artifact)
		}
	}
	ops, err := tx.Operations(landKind, "")
	if err != nil {
		return err
	}
	var landings []Landing
	committed := map[completionLandingKey]bool{}
	for _, op := range ops {
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return err
		}
		if land.Source == nil || land.Source.Execution == nil || !ids[land.Source.Execution.TaskID] {
			continue
		}
		land.State = op.State
		landings = append(landings, land)
		if land.State == LandCommitted && land.Artifact != "" && land.Source.AttemptID != "" {
			committed[completionLandingIdentity(land)] = true
		}
	}
	for _, land := range landings {
		if land.State == LandCommitted {
			continue
		}
		unwritten := land.State == LandMergeConflicted && land.Lease == nil && land.Round == 0 && !land.EndedAt.IsZero() && (land.Unapplied || (land.Now == "" && land.Merged == "" && len(land.Paths) == 0))
		if unwritten && committed[completionLandingIdentity(land)] {
			continue
		}
		return fmt.Errorf("%w: landing %s (%s)", task.ErrCompleteDelivery, land.ID, land.State)
	}
	return nil
}
