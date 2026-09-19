package material

import (
	"encoding/json"

	"github.com/gopact-ai/steve/internal/contentreplica"
)

func init() {
	contentreplica.MustRegisterRetentionOwner(materialKind, materialRetentionRoots)
}

func materialRetentionRoots(key string, raw json.RawMessage, lookup contentreplica.RetentionLookup) ([]string, error) {
	var m *Material
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil || m.ID != key || identity(*m) != key || !digestPattern.MatchString(m.Digest) ||
		m.Size < 0 || m.Size > MaxBlobBytes || !validName(m.Project) || !validName(m.Title) {
		return nil, ErrInvalid
	}
	if err := validateSource(m.Source); err != nil {
		return nil, err
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
		current.Object.Kind != contentreplica.Material || current.Object.Key != m.Digest ||
		current.Object.Blob != (contentreplica.BlobRef{SHA256: m.Digest, Size: m.Size}) {
		return nil, contentreplica.ErrIntegrity
	}
	return []string{m.Content.ID}, nil
}
