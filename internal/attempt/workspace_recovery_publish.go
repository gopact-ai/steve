package attempt

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// PublishRecoveryHead replays only the exact result retained before completion
// failed. It never infers output from disk or from the newest artifact.
func (s *Service) PublishRecoveryHead(ctx context.Context, id string, validate RecoveryArtifactCheck) (WorkspaceRecovery, error) {
	r, err := s.WorkspaceRecovery(ctx, id)
	if err != nil || r.Producer == nil {
		return r, err
	}
	producer, err := s.Get(ctx, r.Producer.Attempt)
	if err != nil {
		return r, err
	}
	if !producer.State.Terminal() {
		return r, nil
	}
	if producer.Unsettled || producer.SessionSettled == nil || !*producer.SessionSettled || producer.Result == nil || producer.Result.RecoveryOutput == nil || producer.Result.CaptureError != "" {
		return r, ErrWorkspaceRecovery
	}
	lease, err := s.l.AcquireIn(ctx, producer.Region, "workspace:"+r.Workspace.ID, "recovery-publication:"+NewID(), s.TTL)
	if err != nil {
		return r, err
	}
	defer s.release(context.WithoutCancel(ctx), lease)
	if err := s.l.CheckAny(ctx, lease); err != nil {
		return r, err
	}
	err = s.l.Update(ctx, func(tx *ledger.Tx) error {
		if lease.Region == "" || lease.Region == s.l.Region() {
			if err := tx.CheckLocalLease(lease); err != nil {
				return err
			}
		}
		current, err := GetTx(tx, producer.ID)
		if err != nil {
			return err
		}
		if current.Revision != producer.Revision || !current.State.Terminal() || current.Unsettled {
			return ledger.ErrConflict
		}
		if err := CheckWriterTx(tx, current.Workspace.Node, current.Workspace.Path); err != nil {
			return err
		}
		var binding *NameBinding
		if current.Result.Artifact != "" {
			proposal := current.Result.RecoveryOutput
			binding = &NameBinding{Name: proposal.Name, ExpectedVersion: proposal.ExpectedVersion}
			if binding.Name == "" {
				return errors.New("recovery output has no exact result name")
			}
			if _, err := tx.CompareAndSetName(binding.Name, binding.ExpectedVersion, current.Result.Artifact); err != nil {
				return err
			}
		}
		if err := completeWorkspaceRecoveryTx(tx, current, binding, validate); err != nil {
			return err
		}
		r, err = recoveryByIDTx(tx, id)
		return err
	})
	return r, err
}
