package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// RecoveryReport distinguishes interrupted work that can be retried from
// writers whose old process still needs explicit stop confirmation.
type RecoveryReport struct {
	Quarantined []Record
	Expired     []Record
}

// PrepareRecovery runs before a new hub admits executions or restores plans.
// Hub death proves nothing about its local or remote child processes. Only a
// durable settled-session fact permits an unfinished attempt's leases to be
// released; absent evidence keeps that attempt quarantined. Bound output is
// already committed and is never invalidated or rerun by this startup pass.
func (s *Service) PrepareRecovery(ctx context.Context, actor string) (RecoveryReport, error) {
	var report RecoveryReport
	live, err := s.Live(ctx)
	if err != nil {
		return report, err
	}
	for _, r := range live {
		if r.Unsettled {
			report.Quarantined = append(report.Quarantined, r)
			continue
		}
		if r.State.Terminal() {
			continue
		}
		if PreparingRelocation(r) {
			if err := s.MarkUnsettled(ctx, r.ID, actor, errors.New("approved relocation preparation awaits its original idempotent open"), nil); err != nil {
				return report, err
			}
			current, err := s.Get(ctx, r.ID)
			if err != nil {
				return report, err
			}
			report.Quarantined = append(report.Quarantined, current)
			continue
		}
		if retainedKind(r.Kind) && retainedPhase(r.State) && strings.HasPrefix(r.Session, "ns_") {
			// The node may hold either an active prompt or a settled result
			// whose completion was not committed before coordinator loss.
			// Preserve its exact leases until authenticated reattachment.
			if err := s.MarkUnsettled(ctx, r.ID, actor, errors.New("retained node session awaits reattachment"), nil); err != nil {
				return report, err
			}
			current, err := s.Get(ctx, r.ID)
			if err != nil {
				return report, err
			}
			report.Quarantined = append(report.Quarantined, current)
			continue
		}
		if r.SessionSettled != nil && *r.SessionSettled {
			if err := s.expireSettled(ctx, r, actor, "previous hub stopped after confirmed session settlement"); err != nil {
				return report, fmt.Errorf("prepare recovery %s: %w", r.ID, err)
			}
			for _, lease := range r.Leases {
				if err := s.l.ReleaseAny(ctx, lease); err != nil && !errors.Is(err, ledger.ErrStale) {
					return report, fmt.Errorf("release settled attempt %s: %w", r.ID, err)
				}
			}
			current, err := s.Get(ctx, r.ID)
			if err != nil {
				return report, err
			}
			report.Expired = append(report.Expired, current)
			continue
		}
		if err := s.MarkUnsettled(ctx, r.ID, actor, errors.New("previous hub ended without session stop confirmation"), nil); err != nil {
			return report, fmt.Errorf("quarantine previous attempt %s: %w", r.ID, err)
		}
		current, err := s.Get(ctx, r.ID)
		if err != nil {
			return report, err
		}
		report.Quarantined = append(report.Quarantined, current)
	}
	return report, nil
}

// ArmSession records possible external execution before issuing another
// prompt or verification command. Running transitions arm automatically.
func (s *Service) ArmSession(ctx context.Context, id, actor string) error {
	return s.sessionEvidence(ctx, id, actor, false)
}

// MarkSessionSettled records an explicit final/cancelled prompt response or
// verified process exit. A generic RPC error/EOF or a local Close bookkeeping
// operation is not evidence. This does not clear an existing quarantine.
func (s *Service) MarkSessionSettled(ctx context.Context, id, actor string) error {
	return s.sessionEvidence(ctx, id, actor, true)
}

func (s *Service) sessionEvidence(ctx context.Context, id, actor string, settled bool) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.State.Terminal() {
		return fmt.Errorf("attempt %s is already %s", id, current.State)
	}
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), actor, current.Leases, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var next Record
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if next.Unsettled {
			return errors.New("quarantined writer requires explicit physical stop confirmation")
		}
		if !settled {
			if err := checkAdmissionTx(tx, next.Spec); err != nil {
				return err
			}
			if err := task.CheckExecutionTx(tx, next.Execution); err != nil {
				return err
			}
		}
		next.SessionSettled = &settled
		return tx.SetData(op, next)
	})
	return err
}
