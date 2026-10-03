package attempt

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/workspacepath"
)

// potentialWriter is independent of TTL: expiry is not physical exit.
func potentialWriter(r Record) bool {
	return r.Unsettled || (!r.State.Terminal() && (r.SessionSettled == nil || !*r.SessionSettled))
}

// A completed native stop does not retire the old conversation binding. Until
// that projection commits, replacement work must not reuse its stale context.
func abandonProjectionPending(r Record) bool {
	return r.Abandoned != nil && r.Abandoned.ProjectedAt.IsZero()
}

func samePhysicalPath(a, b string) bool {
	return workspacepath.Same(a, b)
}

func lostOwnLease(tx *ledger.Tx, r Record) (bool, error) {
	for _, lease := range r.Leases {
		if lease.Key != "attempt:"+r.ID {
			continue
		}
		err := tx.CheckLocalLease(lease)
		if err == nil {
			return false, nil
		}
		if errors.Is(err, ledger.ErrStale) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// StopUnconfirmed names the execution that must be reconciled before
// another writer can be admitted.
type StopUnconfirmed struct{ Holder string }

func (e StopUnconfirmed) Error() string {
	return fmt.Sprintf("%s: attempt %s may still be writing; reconcile it before replacing its work", ErrStopConfirmationRequired, e.Holder)
}

func (e StopUnconfirmed) Unwrap() error { return ErrStopConfirmationRequired }

func writerRefusal(r Record) error { return StopUnconfirmed{Holder: r.ID} }

// checkAdmissionTx closes the gap between an early Open check and the exact
// transaction that arms a new prompt or command. Healthy parallel attempts
// can share a task and use spare endpoint slots; a physical writer cannot.
func checkAdmissionTx(tx *ledger.Tx, spec Spec) error {
	if err := RecoveryHoldTx(tx, spec.Workspace.Node, spec.Workspace.Path); err != nil {
		return err
	}
	if err := checkRecoveryWriterTx(tx, spec); err != nil {
		return err
	}
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return err
		}
		if r.ID == spec.ID || !potentialWriter(r) && !abandonProjectionPending(r) {
			continue
		}
		sameWriter := samePhysicalPath(r.Workspace.Path, spec.Workspace.Path) && r.Workspace.Node == spec.Workspace.Node
		if r.Abandoned != nil && !r.Abandoned.ProjectedAt.IsZero() && !sameWriter {
			continue
		}
		sameTask := spec.TaskID != "" && spec.TaskID == r.TaskID
		sameEndpoint := spec.Slots > 0 && spec.Node == r.Node && spec.Harness == r.Harness
		if !sameWriter && !sameTask && !sameEndpoint {
			continue
		}
		if r.Unsettled || abandonProjectionPending(r) {
			return writerRefusal(r)
		}
		if sameWriter {
			lost, err := lostOwnLease(tx, r)
			if err != nil {
				return err
			}
			if lost {
				return writerRefusal(r)
			}
			// Preserve the ordinary busy response while the physical fence
			// is still valid; a stale physical fence requires stop proof.
			physicalKey := "workspace:" + r.Workspace.ID
			if r.Workspace.Kind == project.KindCanonical {
				physicalKey = "canonical:" + r.Project
			}
			if r.Workspace.Kind == project.KindCopy {
				physicalKey = r.Workspace.ID
			}
			for _, lease := range r.Leases {
				if lease.Key == physicalKey && tx.CheckLocalLease(lease) == nil {
					return Busy{Resource: physicalKey, Holder: r.ID, Until: lease.ExpiresAt}
				}
			}
			return writerRefusal(r)
		}
		lost, err := lostOwnLease(tx, r)
		if err != nil {
			return err
		}
		if lost {
			return writerRefusal(r)
		}
	}
	return nil
}

func (s *Service) checkUnsettled(ctx context.Context, spec Spec) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error { return checkAdmissionTx(tx, spec) })
}

// CheckWriterTx protects a physical directory even before a sweeper marks an
// expired writer. The sole exception is the still-live owner whose canonical
// lease the caller explicitly borrows and validates in the same transition.
// Source metadata is not authority to supply an authorizedAttemptID.
func CheckWriterTx(tx *ledger.Tx, node, path string, authorizedAttemptID ...string) error {
	if err := RecoveryHoldTx(tx, node, path); err != nil {
		return err
	}
	allowed := ""
	if len(authorizedAttemptID) > 0 {
		allowed = authorizedAttemptID[0]
	}
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return err
		}
		if !potentialWriter(r) && !abandonProjectionPending(r) || !samePhysicalPath(r.Workspace.Path, path) || r.Workspace.Node != node {
			continue
		}
		if r.ID == allowed && !r.Unsettled && !abandonProjectionPending(r) {
			lost, err := lostOwnLease(tx, r)
			if err != nil {
				return err
			}
			if !lost {
				continue
			}
		}
		return writerRefusal(r)
	}
	return nil
}
