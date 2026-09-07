package contentreplica

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"

	"github.com/gopact-ai/steve/internal/ledger"
)

const ManifestKind = "content-manifest"

// Record commits only a complete manifest in the same transaction as the
// consumer's metadata. Repeated preparation of an object can add receipts;
// the content identity, classification and byte reference remain immutable.
func Record(tx *ledger.Tx, m Manifest) (Manifest, error) {
	if err := validateManifest(m, math.MaxInt64-1); err != nil {
		return Manifest{}, err
	}
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, ManifestKind, m.ID).Scan(&raw)
	if err == nil {
		var old Manifest
		if json.Unmarshal([]byte(raw), &old) != nil || validateManifest(old, math.MaxInt64-1) != nil || old.Object != m.Object {
			return Manifest{}, ErrIntegrity
		}
		if old.RequiredCopies > m.RequiredCopies {
			m.RequiredCopies, m.Protection = old.RequiredCopies, old.Protection
		}
		byNode := map[string]Receipt{}
		for _, receipt := range old.Receipts {
			byNode[receipt.NodeID] = receipt
		}
		for _, receipt := range m.Receipts {
			byNode[receipt.NodeID] = receipt
		}
		m.Receipts = nil
		for _, receipt := range byNode {
			m.Receipts = append(m.Receipts, receipt)
		}
		sort.Slice(m.Receipts, func(i, j int) bool { return m.Receipts[i].NodeID < m.Receipts[j].NodeID })
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Manifest{}, err
	}
	if err := validateManifest(m, math.MaxInt64-1); err != nil {
		return Manifest{}, err
	}
	return m, tx.PutBinding(ManifestKind, m.ID, m)
}

func Lookup(ctx context.Context, book *ledger.Ledger, id string) (Manifest, bool, error) {
	var m Manifest
	ok, err := book.GetBinding(ctx, ManifestKind, id, &m)
	if err != nil || !ok {
		return m, ok, err
	}
	if err := validateManifest(m, math.MaxInt64-1); err != nil {
		return Manifest{}, false, err
	}
	if m.ID != id {
		return Manifest{}, false, ErrIntegrity
	}
	return m, true, nil
}
