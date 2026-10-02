package artifact

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func artifactTx(tx ledger.Reader, id string) (Manifest, error) {
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, manifestKind, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Manifest{}, contentreplica.ErrIncomplete
		}
		return Manifest{}, err
	}
	var m Manifest
	if json.Unmarshal([]byte(raw), &m) != nil || !gitrepo.ValidSHA(id) || m.ID != id || m.Project == "" || !m.Label.OrDefault().Valid() {
		return Manifest{}, contentreplica.ErrIntegrity
	}
	if m.Content != nil {
		chain, err := contentreplica.ClosureTx(tx, m.Content.ID)
		if err != nil {
			return Manifest{}, err
		}
		current := chain[len(chain)-1]
		if current.Object != m.Content.Object || current.Object.Scope.ProjectID != m.Project || current.Object.Kind != contentreplica.GitBundle || current.Object.Key != m.ID || !m.Label.OrDefault().Admits(datalevel.Level(current.Object.Scope.Level)) {
			return Manifest{}, contentreplica.ErrIntegrity
		}
		m.Content, m.Protection = &current, current.Protection
	}
	return m, nil
}

func (s *Store) storageEvidence(p project.Project, m Manifest) (contentreplica.GitStorageEvidence, error) {
	e := contentreplica.GitStorageEvidence{Project: p.ID, Artifact: m.ID, Level: string(m.Label.OrDefault()), HomeNode: p.Home.Node}
	if m.Project != p.ID {
		return e, contentreplica.ErrIntegrity
	}
	switch {
	case m.Content != nil:
		e.Storage, e.ContentID = "replicated", m.Content.ID
	case metadataOnly(p) && m.Protection == contentreplica.SealedHome:
		e.Storage = "sealed-home"
	case s.replication == nil && m.Protection == "":
		e.Storage = "standalone"
	default:
		return e, contentreplica.ErrIntegrity
	}
	return e, e.Validate()
}

// Evidence is retained for the lifetime of its immutable accepted artifact.
// Recovery owners reference it independently of the ordinary artifact catalog;
// deleting that catalog row must not remove this evidence or its recovery root.
func (s *Store) recordStorageEvidenceTx(tx *ledger.Tx, p project.Project, m Manifest) (string, error) {
	e, err := s.storageEvidence(p, m)
	if err != nil {
		return "", err
	}

	id := e.ID()
	var raw string
	err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, contentreplica.GitStorageEvidenceKind, id).Scan(&raw)
	if err == nil {
		existing, err := contentreplica.LookupGitStorageEvidence(tx, id)
		if err != nil || existing != e {
			return "", contentreplica.ErrIntegrity
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return id, tx.PutBinding(contentreplica.GitStorageEvidenceKind, id, e)
}

func acceptedWorkspaceArtifactTx(tx ledger.Reader, p project.Project, id string) error {
	m, err := artifactTx(tx, id)
	if err != nil {
		return err
	}
	if m.Project != p.ID || !m.Label.OrDefault().Admits(p.Level.OrDefault()) || !m.Durable(p) || m.Content != nil && m.Content.Object.Scope.Level != string(p.Level.OrDefault()) {
		return contentreplica.ErrIntegrity
	}
	return nil
}
