package artifact

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// RecoveryBaselineTx reads an already durable named base. It never snapshots the
// original directory: that directory may still be changed by its old process.
func (s *Store) RecoveryBaselineTx(tx *ledger.Tx, p project.Project) (attempt.RecoveryBaseline, error) {
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
	ref, found, err := tx.Name(CanonicalRef(p.ID))
	if err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	if !found || ref.Version < 1 || !gitrepo.ValidSHA(ref.Artifact) {
		return attempt.RecoveryBaseline{}, fmt.Errorf("project %s has no named recovery base", p.ID)
	}
	m, err := artifactTx(tx, ref.Artifact)
	if err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	if _, err := s.recordStorageEvidenceTx(tx, p, m); err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	content, err := s.RecoveryOutputTx(tx, p.ID, ref.Artifact, ref.Artifact)
	if err != nil {
		return attempt.RecoveryBaseline{}, err
	}
	return attempt.RecoveryBaseline{Name: ref.Name, Version: ref.Version, Artifact: ref.Artifact, ContentID: content.ID, Storage: content.Storage, Evidence: content.Evidence}, nil
}

// RecoveryOutputTx accepts one owner-decoded durable artifact and its parent.
// Missing replicated content is not interpreted as a standalone artifact.
func (s *Store) RecoveryOutputTx(tx ledger.Reader, projectID, parent, id string) (attempt.RecoveryContent, error) {
	p, err := project.ReadTx(tx, projectID)
	if err != nil {
		return attempt.RecoveryContent{}, err
	}
	m, err := artifactTx(tx, id)
	if err != nil {
		return attempt.RecoveryContent{}, err
	}
	if m.Project != p.ID || m.Label.OrDefault() != p.Level.OrDefault() || !m.Durable(p) || id != parent && m.Parent != parent {
		return attempt.RecoveryContent{}, contentreplica.ErrIntegrity
	}
	e, err := s.storageEvidence(p, m)
	if err != nil {
		return attempt.RecoveryContent{}, err
	}
	actual, err := contentreplica.LookupGitStorageEvidence(tx, e.ID())
	if err != nil || actual != e {
		return attempt.RecoveryContent{}, contentreplica.ErrIntegrity
	}
	return attempt.RecoveryContent{ID: e.ContentID, Storage: e.Storage, Evidence: e.ID()}, nil
}
