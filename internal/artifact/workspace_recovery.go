package artifact

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// RecoveryBaselineTx reads an already durable named base. It never snapshots the
// original directory: that directory may still be changed by its old process.
func RecoveryBaselineTx(tx *ledger.Tx, p project.Project) (attempt.RecoveryBaseline, error) {
	operations, err := tx.Operations(landKind, "")
	if err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	for _, operation := range operations {
		if operation.State != LandApplying && operation.State != LandRecoveryPending {
			continue
		}
		var landing Landing
		if err := json.Unmarshal(operation.Data, &landing); err != nil {
			return attempt.RecoveryBaseline{}, err
		}
		if landing.Project == p.ID {
			return attempt.RecoveryBaseline{}, errors.New("canonical landing must settle before workspace recovery can be recorded")
		}
	}
	name := CanonicalRef(p.ID)
	ref, found, err := tx.Name(name)
	if err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	if !found || ref.Version < 1 || !gitrepo.ValidSHA(ref.Artifact) {
		return attempt.RecoveryBaseline{}, fmt.Errorf("project %s has no named recovery base", p.ID)
	}
	if err := CheckRecoveryArtifactTx(tx, p, ref.Artifact); err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	return attempt.RecoveryBaseline{Name: name, Version: ref.Version, Artifact: ref.Artifact}, nil
}

// CheckRecoveryOutputTx keeps continuation history attached to its fixed parent.
func CheckRecoveryOutputTx(tx ledger.Reader, p project.Project, parent, id string) error {
	if err := CheckRecoveryArtifactTx(tx, p, id); err != nil {
		return err
	}
	if id == parent {
		return nil
	}
	var raw []byte
	if err := tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", manifestKind, id).Scan(&raw); err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Parent != parent {
		return errors.New("recovery output has another parent")
	}
	return nil
}

// CheckRecoveryArtifactTx accepts only an owner-decoded durable project artifact.
func CheckRecoveryArtifactTx(tx ledger.Reader, p project.Project, id string) error {
	var raw []byte
	if err := tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", manifestKind, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recovery artifact %s is not recorded", id)
		}
		return err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if !gitrepo.ValidSHA(id) || m.ID != id || m.Project != p.ID || m.Label.OrDefault() != p.Level.OrDefault() || !m.Durable(p) {
		return errors.New("recovery base is not a durable artifact of the declared project")
	}
	return nil
}
