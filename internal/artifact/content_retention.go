package artifact

import (
	"encoding/json"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/contentreplica"
)

func init() {
	contentreplica.MustRegisterRetentionOwner(manifestKind, artifactRetentionRoots)
	contentreplica.MustRegisterRetentionOwner(artifactContentKind, artifactIndexRetentionRoots)
}

func artifactRetentionRoots(key string, raw json.RawMessage, lookup contentreplica.RetentionLookup) ([]string, error) {
	// Decode the whole real owner type, preserving encoding/json's casing,
	// escaped field names and duplicate-field merge behavior.
	var m *Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil || m.ID != key || !gitrepo.ValidSHA(m.ID) || m.Project == "" ||
		!m.Label.OrDefault().Valid() || m.Parent != "" && !gitrepo.ValidSHA(m.Parent) {
		return nil, contentreplica.ErrIntegrity
	}
	if m.Content == nil {
		return nil, nil
	}
	if !m.Content.Complete() {
		return nil, contentreplica.ErrIntegrity
	}
	current, found, err := lookup(m.Content.ID)
	if err != nil {
		return nil, err
	}
	if !found || current.Object != m.Content.Object || current.Object.Scope.ProjectID != m.Project ||
		current.Object.Kind != contentreplica.GitBundle || current.Object.Key != m.ID {
		return nil, contentreplica.ErrIntegrity
	}
	return []string{m.Content.ID}, nil
}

func artifactIndexRetentionRoots(key string, raw json.RawMessage, lookup contentreplica.RetentionLookup) ([]string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	project, commit, ok := strings.Cut(key, "/")
	if !ok || project == "" || !gitrepo.ValidSHA(commit) {
		return nil, contentreplica.ErrIntegrity
	}
	m, found, err := lookup(id)
	if err != nil {
		return nil, err
	}
	if !found || m.Object.Scope.ProjectID != project || m.Object.Kind != contentreplica.GitBundle || m.Object.Key != commit {
		return nil, contentreplica.ErrIntegrity
	}
	return []string{id}, nil
}
