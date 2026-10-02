package attempt

import (
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// RecoveryPublisherTx proves the active writer before artifact reads its copy.
// The artifact owner separately checks any leases issued by another region.
func RecoveryPublisherTx(tx *ledger.Tx, id string, ws project.Workspace, base, region string) (Record, error) {
	r, err := GetTx(tx, id)
	if err != nil {
		return r, err
	}
	if r.WorkspaceRecovery == nil || r.WorkspaceRecovery.ID != ws.RecoveryID || !sameRecoveryWorkspace(r.Workspace, ws) || r.Base != base || r.Unsettled || r.State.Terminal() || r.Execution == nil {
		return Record{}, ErrWorkspaceRecovery
	}
	episode, err := recoveryByIDTx(tx, r.WorkspaceRecovery.ID)
	if err != nil {
		return r, err
	}
	if episode.Producer == nil || episode.Producer.Attempt != id || episode.Producer.Execution != *r.Execution || episode.Head.Version != r.WorkspaceRecovery.HeadVersion || episode.Head.Artifact != base {
		return Record{}, ErrWorkspaceRecovery
	}
	if err := task.CheckExecutionTx(tx, r.Execution); err != nil {
		return Record{}, err
	}
	if err := checkRecoveryDeclarationTx(tx, episode); err != nil {
		return Record{}, err
	}
	found := false
	for _, lease := range r.Leases {
		if lease.Key == "workspace:"+ws.ID {
			found = true
		}
		if lease.Region == "" || lease.Region == region {
			if err := tx.CheckLocalLease(lease); err != nil {
				return Record{}, err
			}
		}
	}
	if !found {
		return Record{}, errors.New("recovery publisher has no workspace lease")
	}
	return r, nil
}
