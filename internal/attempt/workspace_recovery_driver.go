package attempt

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// PrepareWorkspaceRecovery owns preparation through a renewable control lease.
// Checkout stays outside the ledger transaction; readiness is fenced on this
// exact lease after the verified filesystem operation has returned.
func (s *Service) PrepareWorkspaceRecovery(parent context.Context, expected WorkspaceRecovery, prepare func(context.Context, WorkspaceRecovery) error) (WorkspaceRecovery, error) {
	if expected.Phase == "ready" || expected.Phase == "working" {
		return expected, nil
	}
	lease, err := s.l.Acquire(parent, "workspace-recovery-driver:"+expected.ID, NewID(), s.TTL)
	if err != nil {
		return WorkspaceRecovery{}, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(max(s.TTL/3, time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := s.l.RenewAny(ctx, lease, s.TTL); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	defer func() { cancel(context.Canceled); <-stopped; s.release(context.WithoutCancel(parent), lease) }()
	current, err := s.WorkspaceRecovery(ctx, expected.ID)
	if err != nil {
		return WorkspaceRecovery{}, err
	}
	if current.Workspace != expected.Workspace {
		return WorkspaceRecovery{}, ledger.ErrConflict
	}
	if current.Phase == "ready" || current.Phase == "working" {
		return current, nil
	}
	if prepare == nil {
		return WorkspaceRecovery{}, errors.New("recovery preparation is not configured")
	}
	if err := prepare(ctx, current); err != nil {
		return WorkspaceRecovery{}, err
	}
	var result WorkspaceRecovery
	err = s.l.Update(ctx, func(tx *ledger.Tx) error {
		if err := tx.CheckLocalLease(lease); err != nil {
			return err
		}
		r, err := recoveryByIDTx(tx, current.ID)
		if err != nil {
			return err
		}
		if r.Revision != current.Revision || r.Phase != "materializing" || r.Workspace != current.Workspace {
			return ledger.ErrConflict
		}
		if err := checkRecoveryDeclarationTx(tx, r); err != nil {
			return err
		}
		r.Phase = "ready"
		if err := saveWorkspaceRecoveryTx(tx, &r, "workspace-prepared"); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}
