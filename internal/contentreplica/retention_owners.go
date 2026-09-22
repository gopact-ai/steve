package contentreplica

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// RetentionLookup reads the manifest catalog from the same ledger transaction
// as the owner row. Decoders must not query a separate ledger snapshot.
type RetentionLookup func(string) (Manifest, bool, error)

// RetentionOwnerDecoder validates the complete binding using its owner's real
// type and decoder semantics, then returns all referenced manifest IDs. An
// unreadable owner must return an error, never an empty set of references.
type RetentionOwnerDecoder func(key string, raw json.RawMessage, lookup RetentionLookup) ([]string, error)

var retentionOwners = struct {
	sync.Mutex
	frozen   bool
	decoders map[string]RetentionOwnerDecoder
}{
	// These are the protocol's content-bearing binding kinds, not copies of
	// their schemas. A binary without an owner adapter cannot collect a
	// catalog containing that owner's rows.
	decoders: map[string]RetentionOwnerDecoder{
		"artifact":         nil,
		"artifact-content": nil,
		"material":         nil,
		"plugin-package":   nil,
	},
}

// MustRegisterRetentionOwner is called by the owning package during init.
// Missing adapters fail closed; duplicate or late registrations panic.
func MustRegisterRetentionOwner(kind string, decoder RetentionOwnerDecoder) {
	retentionOwners.Lock()
	defer retentionOwners.Unlock()
	current, known := retentionOwners.decoders[kind]
	if retentionOwners.frozen || !known || current != nil || decoder == nil {
		panic("contentreplica: invalid retention owner registration " + kind)
	}
	retentionOwners.decoders[kind] = decoder
}

func retentionOwnerDecoders() map[string]RetentionOwnerDecoder {
	retentionOwners.Lock()
	defer retentionOwners.Unlock()
	retentionOwners.frozen = true
	return maps.Clone(retentionOwners.decoders)
}

type retentionOwnerRow struct {
	kind string
	key  string
	raw  json.RawMessage
}

func (c retentionCatalog) protectOwners(decoders map[string]RetentionOwnerDecoder) error {
	for _, row := range c.owners {
		decode := decoders[row.kind]
		if decode == nil {
			return fmt.Errorf("%w: retention owner %s/%s has no registered decoder", ErrIntegrity, row.kind, row.key)
		}
		roots, err := decode(row.key, row.raw, c.loadManifest)
		if err != nil {
			return fmt.Errorf("%w: retention owner %s/%s: %w", ErrIntegrity, row.kind, row.key, err)
		}
		for _, id := range roots {
			if _, ok := c.manifests[id]; !ok {
				return fmt.Errorf("%w: retention owner %s/%s references missing manifest %s", ErrIntegrity, row.kind, row.key, id)
			}
			c.roots[id] = true
		}
	}
	return nil
}
