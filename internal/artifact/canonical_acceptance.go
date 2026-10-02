package artifact

import (
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type recordGuard struct {
	check    func(*ledger.Tx) error
	accepted func(*ledger.Tx, Manifest) error
	lease    *ledger.Lease
}

func (s *Store) canonicalAcceptance(expected project.Project, held ledger.Lease, artifact string, named bool, actor string) recordGuard {
	return recordGuard{lease: &held, check: func(tx *ledger.Tx) error {
		if held.Key != canonicalLock(expected.ID) {
			return contentreplica.ErrIntegrity
		}
		if held.Region == "" || held.Region == s.ledger.Region() {
			if err := tx.CheckLocalLease(held); err != nil {
				return err
			}
		}
		current, err := project.ReadTx(tx, expected.ID)
		if errors.Is(err, project.ErrUnknown) {
			current, err = project.ReadHistoricalTx(tx, expected.ID)
			if err == nil {
				err = checkHistoricalSnapshotTx(tx, current, held, actor)
			}
		}
		if err != nil {
			return err
		}
		if project.RecoveryIdentity(current) != project.RecoveryIdentity(expected) {
			return contentreplica.ErrIntegrity
		}
		if err := attempt.RecoveryHoldTx(tx, expected.Home.Node, expected.Home.Path); err != nil {
			return err
		}
		if !named {
			return nil
		}
		ref, _, err := tx.Name(CanonicalRef(expected.ID))
		if err != nil {
			return err
		}
		if ref.Artifact == artifact {
			return nil
		}
		_, err = tx.CompareAndSetName(CanonicalRef(expected.ID), ref.Version, artifact)
		return err
	}}
}

// Only an already admitted, exact landing WAL can consume a retired Target.
// A caller-provided actor string alone never grants this historical exception.
func checkHistoricalSnapshotTx(tx ledger.Reader, p project.Project, held ledger.Lease, id string) error {
	var raw, state string
	if err := tx.QueryRow(`SELECT data,state FROM operations WHERE kind=? AND id=?`, landKind, id).Scan(&raw, &state); err != nil {
		return err
	}
	var land Landing
	if json.Unmarshal([]byte(raw), &land) != nil || land.ID != id || land.Project != p.ID || land.Target != p.Home || state != LandApplying && state != LandRecoveryPending || !sameLease(land.Lease, held) {
		return contentreplica.ErrIntegrity
	}
	return nil
}
