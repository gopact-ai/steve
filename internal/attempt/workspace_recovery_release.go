package attempt

import (
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// RecoveryResolutionRef is the immutable conflict selected before a resolver
// starts. A restarted sink cannot replace it with a newer episode or conflict.
type RecoveryResolutionRef struct {
	Episode     string `json:"episode"`
	HeadVersion int64  `json:"head_version"`
	Root        string `json:"root"`
	Conflict    string `json:"conflict"`
	Marked      string `json:"marked"`
	Canonical   string `json:"canonical"`
}

type RecoveryResult struct {
	Artifact string `json:"artifact"`
	Version  int64  `json:"version"`
	Landing  string `json:"landing"`
	Outcome  string `json:"outcome"`
	RecoveryContent
}

// ReleaseRecoveryTx composes the artifact owner's committed result with the
// sole original-hold transition. It never mints an old task token.
func ReleaseRecoveryTx(tx *ledger.Tx, id string, version int64, driver ledger.Lease, result RecoveryResult, at time.Time) error {
	r, err := checkRecoveryDriverTx(tx, id, driver)
	if err != nil {
		return err
	}
	if r.Phase != "landing" || r.Head.Version != version || r.FrozenHeadVersion != version || r.Residual == nil || at.IsZero() || result.Landing == "" || result.Version < 1 || result.Outcome == "" {
		return ErrWorkspaceRecovery
	}
	if err := recoveryConvergedTx(tx, r); err != nil {
		return err
	}
	for _, source := range r.Sources {
		original, err := GetTx(tx, source.Attempt)
		if err != nil {
			return err
		}
		if original.Abandoned == nil || original.Abandoned.ProjectedAt.IsZero() || original.Abandoned.DeliveryDoneAt.IsZero() {
			return errors.New("recovery awaits the original abandonment projection or delivery")
		}
	}
	e, err := contentreplica.LookupGitStorageEvidence(tx, result.Evidence)
	if err != nil {
		return err
	}
	if e.Project != r.Project || e.Artifact != result.Artifact || e.ContentID != result.ID || e.Storage != result.Storage {
		return contentreplica.ErrIntegrity
	}
	r.Result, r.ReleasedAt, r.Phase = &result, at, "released"
	return saveWorkspaceRecoveryTx(tx, &r, "recovery-released")
}

// RecoveryResolutionPublicationTx permits only a normal resolver's isolated
// publication against the exact current conflict reference, never target I/O.
func RecoveryResolutionPublicationTx(tx *ledger.Tx, ref RecoveryResolutionRef) (WorkspaceRecovery, error) {
	r, err := recoveryByIDTx(tx, ref.Episode)
	if err != nil {
		return r, err
	}
	if r.Phase != "landing" || r.Residual == nil || r.Producer != nil || r.FrozenHeadVersion != ref.HeadVersion || r.Head.Version != ref.HeadVersion || ref.Root != RecoveryLandingID(r) {
		return r, ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, r); err != nil {
		return r, err
	}
	if err := recoveryConvergedTx(tx, r); err != nil {
		return r, err
	}
	for _, source := range r.Head.Sources {
		if err := task.CheckExecutionTx(tx, &source.Execution); err != nil {
			return r, err
		}
	}
	return r, nil
}

type RecoveryResolver struct {
	Landing   string              `json:"landing"`
	Attempt   string              `json:"attempt"`
	Execution task.ExecutionToken `json:"execution"`
	Artifact  string              `json:"artifact"`
	RecoveryContent
}

// RetainRecoveryResolverTx is the narrow deletion guard for an accepted D
// whose normal resolver task must remain usable until the episode releases.
func RetainRecoveryResolverTx(tx *ledger.Tx, id string, version int64, driver ledger.Lease, resolver RecoveryResolver) error {
	r, err := checkRecoveryDriverTx(tx, id, driver)
	if err != nil {
		return err
	}
	if r.Phase != "landing" || r.FrozenHeadVersion != version || resolver.Attempt == "" || resolver.Landing == "" || resolver.Execution.TaskID == "" {
		return ErrWorkspaceRecovery
	}
	if err := task.CheckExecutionTx(tx, &resolver.Execution); err != nil {
		return err
	}
	record, err := GetTx(tx, resolver.Attempt)
	if err != nil {
		return err
	}
	if record.Execution == nil || *record.Execution != resolver.Execution || record.Project != r.Project || record.Result == nil || record.Result.Artifact != resolver.Artifact || !record.State.Terminal() {
		return ErrWorkspaceRecovery
	}
	for _, prior := range r.Resolvers {
		if prior.Landing == resolver.Landing {
			if prior != resolver {
				return ledger.ErrConflict
			}
			return nil
		}
	}
	r.Resolvers = append(r.Resolvers, resolver)
	return saveWorkspaceRecoveryTx(tx, &r, "recovery-resolver")
}
