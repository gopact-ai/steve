package artifact

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

// ReplayAdmittedRecovery consumes only the frozen episode's already applying
// or committed WAL. It cannot create a root, resolver or capture operation.
func (s *Store) ReplayAdmittedRecovery(ctx context.Context, id string) (bool, error) {
	r, err := attempt.New(s.ledger).WorkspaceRecovery(ctx, id)
	if err != nil {
		return false, err
	}
	if r.Phase != "landing" {
		return false, nil
	}
	var candidate *Landing
	if err := s.ledger.Read(ctx, func(tx *ledger.ReadTx) error {
		winner, err := recoveryWinnerTx(tx, r)
		if err == nil {
			candidate = &winner
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	}); err != nil {
		return false, err
	}
	if candidate == nil {
		all, err := s.Landings(ctx, r.Project)
		if err != nil {
			return false, err
		}
		for _, land := range all {
			if land.Recovery == nil || land.Recovery.Episode != r.ID {
				continue
			}
			if land.Recovery.HeadVersion != r.FrozenHeadVersion || land.Target != r.Target {
				return false, attempt.ErrWorkspaceRecovery
			}
			if land.State != LandApplying && land.State != LandRecoveryPending {
				continue
			}
			if candidate != nil {
				return false, errors.New("recovery has competing admitted WALs")
			}
			copy := land
			candidate = &copy
		}
	}
	if candidate == nil {
		return false, nil
	}
	lifetime := ctx
	if s.executions != nil {
		lifetime = s.executions.Detached(ctx)
	}
	lifetime, stop := context.WithTimeout(lifetime, 5*time.Minute)
	defer stop()
	err = attempt.New(s.ledger).DriveWorkspaceRecovery(ctx, lifetime, id, func(ctx context.Context, driver ledger.Lease) error {
		p, found, err := s.projects.Get(ctx, r.Project)
		if err != nil {
			return err
		}
		if !found {
			return attempt.ErrWorkspaceRecovery
		}
		ctx = context.WithValue(ctx, recoveryLandingKey{}, &recoveryLandingPermit{link: *candidate.Recovery, driver: driver, lifetime: attempt.RecoveryDriverLifetime(ctx)})
		if candidate.State != LandCommitted {
			var sources []Source
			if candidate.Source != nil {
				sources = []Source{*candidate.Source}
			}
			land, err := s.LandOnce(ctx, candidate.ID, p, candidate.Artifact, "workspace recovery replay", sources...)
			if err != nil {
				return err
			}
			if land.State != LandCommitted {
				return ErrRecoveryPending
			}
		}
		_, err = s.ReleaseRecovery(ctx, id, driver)
		return err
	})
	return true, err
}
