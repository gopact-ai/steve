package attempt

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

// These are historical accepted producer facts, not permission to run or land.
// Current token epochs and project authorization remain with their own owners.
func validateWorkspaceRecoveryTx(tx ledger.Reader, r WorkspaceRecovery) error {
	if err := validateOriginalRecoverySourcesTx(tx, r); err != nil {
		return err
	}
	check := func(artifact, content, storage, evidence string) error {
		e, err := contentreplica.LookupGitStorageEvidence(tx, evidence)
		if err != nil {
			return err
		}
		if e.Project != r.Project || e.Artifact != artifact || e.ContentID != content || e.Storage != storage {
			return contentreplica.ErrIntegrity
		}
		if storage == "replicated" {
			chain, err := contentreplica.ClosureTx(tx, content)
			if err != nil {
				return err
			}
			m := chain[len(chain)-1]
			if m.Object.Scope.ProjectID != r.Project || m.Object.Kind != contentreplica.GitBundle || m.Object.Key != artifact || m.Object.Scope.Level != e.Level {
				return contentreplica.ErrIntegrity
			}
		}
		return nil
	}
	if err := check(r.Baseline.Artifact, r.Baseline.ContentID, r.Baseline.Storage, r.Baseline.Evidence); err != nil {
		return err
	}
	if err := check(r.Head.Artifact, r.Head.ContentID, r.Head.Storage, r.Head.Evidence); err != nil {
		return err
	}
	if r.Residual != nil {
		if err := check(r.Residual.Artifact, r.Residual.ID, r.Residual.Storage, r.Residual.Evidence); err != nil {
			return err
		}
	}
	if r.Result != nil {
		if err := check(r.Result.Artifact, r.Result.ID, r.Result.Storage, r.Result.Evidence); err != nil {
			return err
		}
	}
	for _, resolver := range r.Resolvers {
		if err := check(resolver.Artifact, resolver.ID, resolver.Storage, resolver.Evidence); err != nil {
			return err
		}
		record, err := GetTx(tx, resolver.Attempt)
		if err != nil {
			return err
		}
		if record.Execution == nil || *record.Execution != resolver.Execution || record.Result == nil || record.Result.Artifact != resolver.Artifact || record.Project != r.Project {
			return contentreplica.ErrIntegrity
		}
	}
	for _, source := range r.Head.Sources {
		if err := check(source.Artifact, source.ContentID, source.Storage, source.Evidence); err != nil {
			return err
		}
		original, err := GetTx(tx, source.Attempt)
		if err != nil {
			return fmt.Errorf("%w: missing recovery producer %s", contentreplica.ErrIntegrity, source.Attempt)
		}
		if !sameRecoveryWorkspace(original.Workspace, r.Workspace) || original.Project != r.Project || original.Execution == nil || *original.Execution != source.Execution || original.TaskID != source.Execution.TaskID || original.Base != source.Base || original.WorkspaceRecovery == nil || original.WorkspaceRecovery.ID != r.ID || original.WorkspaceRecovery.HeadVersion != source.HeadVersion || original.Result == nil || original.Result.Artifact != source.Artifact || original.Result.CaptureError != "" || original.Result.RecoveryOutput == nil || !original.State.Terminal() {
			return contentreplica.ErrIntegrity
		}
	}
	return nil
}
