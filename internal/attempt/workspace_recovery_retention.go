package attempt

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/contentreplica"
)

func init() {
	contentreplica.MustRegisterRetentionOwner(workspaceRecoveryKind, workspaceRecoveryContentRoots)
}

func workspaceRecoveryContentRoots(key string, raw json.RawMessage, lookup contentreplica.RetentionLookup) ([]string, error) {
	var envelope recoveryEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	r, err := decodeWorkspaceRecovery(envelope)
	if err != nil || r.ID != key {
		return nil, fmt.Errorf("%w: recovery identity %s: %v", contentreplica.ErrIntegrity, key, err)
	}
	var roots []string
	protect := func(artifact, content, storage string) error {
		if storage != "replicated" {
			return nil
		}
		m, found, err := lookup(content)
		if err != nil {
			return err
		}
		if !found || !m.Complete() || m.Object.Scope.ProjectID != r.Project || m.Object.Kind != contentreplica.GitBundle || m.Object.Key != artifact {
			return contentreplica.ErrIntegrity
		}
		roots = append(roots, content)
		return nil
	}
	if err := protect(r.Baseline.Artifact, r.Baseline.ContentID, r.Baseline.Storage); err != nil {
		return nil, err
	}
	if err := protect(r.Head.Artifact, r.Head.ContentID, r.Head.Storage); err != nil {
		return nil, err
	}
	for _, source := range r.Head.Sources {
		if err := protect(source.Artifact, source.ContentID, source.Storage); err != nil {
			return nil, err
		}
	}
	return roots, nil
}
