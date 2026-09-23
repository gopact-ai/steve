package pluginledger

import (
	"encoding/json"
	"strings"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/plugins"
)

func init() {
	contentreplica.MustRegisterRetentionOwner(packageRecordKind, packageRetentionRoots)
}

func packageRetentionRoots(key string, raw json.RawMessage, lookup contentreplica.RetentionLookup) ([]string, error) {
	// Match Library.Record's whole-record decoder, including case-folded
	// and duplicate fields; the public bundle parser has a different contract.
	var record *PackageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if record == nil || record.Project == "" || len(record.Project) > 256 ||
		strings.ContainsAny(record.Project, "\x00\r\n") || !plugins.ValidDigest(record.Digest) ||
		libraryKey(record.Project, record.Digest) != key {
		return nil, plugins.ErrIntegrity
	}
	if err := record.Manifest.Validate(); err != nil {
		return nil, err
	}
	if record.Content == nil {
		return nil, nil
	}
	if !record.Content.Complete() {
		return nil, contentreplica.ErrIntegrity
	}
	current, found, err := lookup(record.Content.ID)
	if err != nil {
		return nil, err
	}
	if !found || current.Object != record.Content.Object || current.Object.Scope.ProjectID != record.Project ||
		current.Object.Kind != contentreplica.PluginPackage || current.Object.Key != record.Digest ||
		current.Object.Blob.SHA256 != record.Digest || current.Object.Blob.Size > plugins.MaxPackageBytes {
		return nil, plugins.ErrIntegrity
	}
	return []string{record.Content.ID}, nil
}
