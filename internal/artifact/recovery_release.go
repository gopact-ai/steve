package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrRecoveryResultPending = errors.New("workspace recovery has no committed result yet")

// ReleaseRecovery accepts only this episode's own committed winner. Reading an
// arbitrary later canonical name or the old Merged SHA cannot produce Result.
func (s *Store) ReleaseRecovery(ctx context.Context, id string, driver ledger.Lease) (attempt.WorkspaceRecovery, error) {
	a := attempt.New(s.ledger)
	r, err := a.WorkspaceRecovery(ctx, id)
	if err != nil {
		return r, err
	}
	if r.Phase == "released" {
		return r, nil
	}
	var winner Landing
	if err := s.ledger.Read(ctx, func(tx *ledger.ReadTx) error { var err error; winner, err = recoveryWinnerTx(tx, r); return err }); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrRecoveryResultPending
		}
		return r, err
	}
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return r, err
	}
	if !found {
		return r, attempt.ErrWorkspaceRecovery
	}
	ctx = context.WithValue(ctx, recoveryLandingKey{}, &recoveryLandingPermit{link: *winner.Recovery, driver: driver, lifetime: attempt.RecoveryDriverLifetime(ctx)})
	if err := s.ensureRecoveryLandingReceipt(ctx, p, winner); err != nil {
		return r, err
	}
	err = s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		current, err := attempt.CheckRecoveryLandingTx(tx, id, r.FrozenHeadVersion, driver, nil, false)
		if err != nil {
			return err
		}
		actual, err := recoveryWinnerTx(tx, current)
		if err != nil {
			return err
		}
		if actual.ID != winner.ID || actual.Committed == nil || *actual.Committed != *winner.Committed {
			return ledger.ErrConflict
		}
		if _, err := checkRecoveryLandingPermitTx(ctx, tx, p, &actual, nil, false); err != nil {
			return err
		}
		content, err := s.RecoveryOutputTx(tx, r.Project, actual.Committed.Artifact, actual.Committed.Artifact)
		if err != nil {
			return err
		}
		if err := consumeRecoveryPendingTx(tx, current, actual); err != nil {
			return err
		}
		outcome := "merged"
		if actual.Recovery.Resolution != nil {
			outcome = "resolved"
		} else if len(actual.Paths) == 0 {
			outcome = "no-net-change"
			if len(current.Head.Sources) == 0 {
				outcome = "no-copy-change"
			}
		}
		return attempt.ReleaseRecoveryTx(tx, id, r.FrozenHeadVersion, driver, attempt.RecoveryResult{Artifact: actual.Committed.Artifact, Version: actual.Committed.Version, Landing: actual.ID, Outcome: outcome, RecoveryContent: content}, s.now().UTC())
	})
	if err != nil {
		return r, err
	}
	return a.WorkspaceRecovery(ctx, id)
}

func consumeRecoveryPendingTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, winner Landing) error {
	id := r.Project + "/" + r.Head.Artifact
	var raw []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, pendingKind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if winner.Recovery.Resolution != nil {
			return ErrNotBlocked
		}
		return nil
	}
	if err != nil {
		return err
	}
	var item Pending
	if json.Unmarshal(raw, &item) != nil || item.Recovery == nil || item.Recovery.Episode != r.ID || item.Recovery.HeadVersion != r.FrozenHeadVersion || item.Blocked == nil {
		return ErrNotBlocked
	}
	if winner.Recovery.Resolution == nil || item.Blocked.Landing != winner.Recovery.Resolution.Conflict || item.Resolution == nil || *item.Resolution != *winner.Recovery.Resolution {
		return ErrNotBlocked
	}
	_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, pendingKind, id)
	return err
}
