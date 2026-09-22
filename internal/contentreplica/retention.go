package contentreplica

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/ledger"
)

type retentionCatalog struct {
	manifests map[string]Manifest
	uploads   map[string]uploadRecord
	receipts  map[string]receiptRecord
	releases  map[string]releaseRecord
	roots     map[string]bool
	owners    []retentionOwnerRow
}

type retentionRow interface{ Scan(...any) error }
type retentionQuery func(string, ...any) retentionRow

// Owner rows include historical records, not only current project heads.
// Catalog corruption is checked before any destructive decision.
func readRetentionCatalog(query retentionQuery) (retentionCatalog, error) {
	c := retentionCatalog{
		manifests: map[string]Manifest{},
		uploads:   map[string]uploadRecord{},
		receipts:  map[string]receiptRecord{},
		releases:  map[string]releaseRecord{},
		roots:     map[string]bool{},
	}
	kind, key := "", ""
	for {
		var raw string
		err := query(`SELECT kind, id, data FROM bindings
			WHERE kind IN ('content-manifest','content-upload','content-receipt','content-release','artifact','artifact-content','material','plugin-package')
			AND (kind > ? OR (kind = ? AND id > ?)) ORDER BY kind, id LIMIT 1`, kind, kind, key).Scan(&kind, &key, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return c, err
		}
		if err := c.addRow(kind, key, raw); err != nil {
			return c, err
		}
	}
	high, err := readUploadClock(query)
	if err != nil {
		return c, err
	}
	for id := range c.uploads {
		if uploadSequence(id) > high {
			return c, fmt.Errorf("%w: upload %s exceeds allocation clock", ErrIntegrity, id)
		}
	}
	if err := c.validateManifests(); err != nil {
		return c, err
	}
	if err := c.validateReceipts(); err != nil {
		return c, err
	}
	if err := c.protectOwners(retentionOwnerDecoders()); err != nil {
		return c, err
	}
	if err := c.protectPendingUploads(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *retentionCatalog) addRow(kind, key, raw string) error {
	invalid := func() error { return fmt.Errorf("%w: retention %s/%s", ErrIntegrity, kind, key) }
	switch kind {
	case ManifestKind:
		var m Manifest
		if json.Unmarshal([]byte(raw), &m) != nil || m.ID != key || !m.Complete() {
			return invalid()
		}
		c.manifests[key] = m
	case uploadKind:
		u, err := decodeUpload(key, []byte(raw))
		if err != nil {
			return err
		}
		c.uploads[key] = u
	case receiptKind:
		var r receiptRecord
		if json.Unmarshal([]byte(raw), &r) != nil || validateObject(r.Object, math.MaxInt64-1) != nil ||
			r.Receipt.Key() != key || r.Receipt.ObjectID != r.Object.ID() || uploadSequence(r.Receipt.UploadID) == 0 ||
			!validID(r.Receipt.NodeID) || !validID(r.Receipt.FailureDomain) || r.Receipt.StoredAt.IsZero() {
			return invalid()
		}
		c.receipts[key] = r
	case releaseKind:
		var r releaseRecord
		if json.Unmarshal([]byte(raw), &r) != nil || uploadSequence(r.Upload.ID) == 0 || !validID(r.NodeID) || validateObject(r.Upload.Object, math.MaxInt64-1) != nil {
			return invalid()
		}
		receipt := Receipt{UploadID: r.Upload.ID, ObjectID: r.Upload.Object.ID(), NodeID: r.NodeID}
		if receipt.Key() != key {
			return invalid()
		}
		c.releases[key] = r
	default:
		c.owners = append(c.owners, retentionOwnerRow{kind: kind, key: key, raw: json.RawMessage(raw)})
	}
	return nil
}

func (c retentionCatalog) loadManifest(id string) (Manifest, bool, error) {
	m, ok := c.manifests[id]
	return m, ok, nil
}

func (c retentionCatalog) validateManifests() error {
	for id, m := range c.manifests {
		if _, err := closure(id, c.loadManifest); err != nil {
			return err
		}
		for _, r := range m.Receipts {
			claim, ok := c.receipts[r.Key()]
			if !ok || claim.Released || claim.Object != m.Object || !equalReceipt(claim.Receipt, r) {
				return fmt.Errorf("%w: manifest %s has unconfirmed receipt %s", ErrIntegrity, id, r.Key())
			}
		}
	}
	return nil
}

func (c retentionCatalog) validateReceipts() error {
	for _, r := range c.receipts {
		u, ok := c.uploads[r.Receipt.UploadID]
		if !ok || u.State != "published" || u.Upload.Object != r.Object {
			return ErrIntegrity
		}
		if !r.Released {
			if _, ok := c.manifests[r.Object.ID()]; !ok {
				return fmt.Errorf("%w: unreleased receipt without manifest", ErrIntegrity)
			}
		} else if _, ok := c.releases[r.Receipt.Key()]; !ok {
			return ErrIntegrity
		}
	}
	for key, r := range c.releases {
		u, ok := c.uploads[r.Upload.ID]
		if !ok || u.Upload != r.Upload || !slices.Contains(u.Targets, r.NodeID) || u.State == "pending" {
			return ErrIntegrity
		}
		if u.State != "aborted" && slices.ContainsFunc(u.Receipts, func(receipt Receipt) bool { return receipt.NodeID == r.NodeID }) {
			claim, ok := c.receipts[key]
			if !ok || !claim.Released || claim.Object != r.Upload.Object {
				return ErrIntegrity
			}
		}
	}
	return nil
}

func (c retentionCatalog) protectPendingUploads() error {
	for id, u := range c.uploads {
		if u.State != "pending" && u.State != "prepared" {
			continue
		}
		o := u.Upload.Object
		c.roots[o.ID()] = true
		if o.Base != "" {
			chain, err := closure(o.Base, c.loadManifest)
			if err != nil {
				return fmt.Errorf("pending upload %s: %w", id, err)
			}
			base := chain[len(chain)-1].Object
			if len(chain) >= MaxBundleDepth || base.Scope != o.Scope || base.Kind != GitBundle || base.Key == o.Key {
				return ErrIntegrity
			}
			c.roots[o.Base] = true
		}
	}
	return nil
}

// Retire explicitly removes an unreferenced published object. It closes only
// its confirmed receipt keys, not its ObjectID: a later independent upload of
// the same content remains possible. Unknown pending uploads prevent retirement.
func Retire(tx *ledger.Tx, id string) error {
	if !digest(id, 64) {
		return ErrInvalid
	}
	c, err := readRetentionCatalog(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return err
	}
	if c.roots[id] {
		return ErrReferenced
	}
	for _, m := range c.manifests {
		if m.Object.Base == id {
			return ErrReferenced
		}
	}
	if _, found := c.manifests[id]; !found {
		for _, r := range c.receipts {
			if r.Object.ID() == id && r.Released {
				return nil
			}
		}
		return ErrIncomplete
	}
	for key, r := range c.receipts {
		if r.Object.ID() == id {
			r.Released = true
			if err := tx.PutBinding(receiptKind, key, r); err != nil {
				return err
			}
			if err := releaseReceipt(tx, Upload{ID: r.Receipt.UploadID, Object: r.Object}, r.Receipt.NodeID); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, ManifestKind, id)
	return err
}

// ReleaseSuperseded closes exact old receipts only after their same-node
// replacement has been published. It never infers object retirement from a
// missing owner or a scan of currently active projects.
func ReleaseSuperseded(tx *ledger.Tx) error {
	c, err := readRetentionCatalog(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return err
	}
	for key, claim := range c.receipts {
		if claim.Released {
			continue
		}
		m := c.manifests[claim.Object.ID()]
		for _, current := range m.Receipts {
			if current.NodeID == claim.Receipt.NodeID && current.Key() != key {
				claim.Released = true
				if err := tx.PutBinding(receiptKind, key, claim); err != nil {
					return err
				}
				if err := releaseReceipt(tx, Upload{ID: claim.Receipt.UploadID, Object: claim.Object}, claim.Receipt.NodeID); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// GC applies only committed exact-receipt releases or an explicit abort of
// that upload. Unrecognized and pending uploads remain pinned indefinitely.
// The supplied ledger must be the receiver's committed authoritative catalog.
func (s *Store) GC(ctx context.Context, book *ledger.Ledger) (checkpoint.RetentionGCResult, error) {
	if book == nil || book != s.book {
		return checkpoint.RetentionGCResult{}, ErrInvalid
	}
	inventory, err := s.blobs.Retained(ctx)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	var c retentionCatalog
	err = book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		c, err = readRetentionCatalog(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
		return err
	})
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	release, required, err := s.collectionCandidates(c, inventory)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	return s.blobs.CollectRetained(ctx, release, required)
}

func (s *Store) collectionCandidates(c retentionCatalog, inventory []checkpoint.RetainedBlob) ([]checkpoint.RetainedBlob, []checkpoint.RetainedBlob, error) {
	required, pending := c.localProtection(s.node)
	local := map[string]checkpoint.RetainedBlob{}
	for _, retained := range inventory {
		var owner retainedOwner
		if json.Unmarshal([]byte(retained.Owner), &owner) != nil || uploadSequence(owner.Upload.ID) == 0 ||
			validateObject(owner.Upload.Object, math.MaxInt64-1) != nil || !validID(owner.FailureDomain) {
			return nil, nil, ErrIntegrity
		}
		if retained != retainedUpload(owner.Upload, s.node, owner.FailureDomain) {
			return nil, nil, ErrIntegrity
		}
		local[retained.ID] = retained
	}
	keys := make([]string, 0, len(c.releases))
	for key, proof := range c.releases {
		if proof.NodeID == s.node && !proof.Collected {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var release []checkpoint.RetainedBlob
	for _, key := range keys {
		proof := c.releases[key]
		object := proof.Upload.Object
		if pending[retentionBlob{scope: object.Scope, blob: object.Blob}] {
			continue
		}
		retained, present := local[key]
		if present {
			var owner retainedOwner
			if json.Unmarshal([]byte(retained.Owner), &owner) != nil || owner.Upload != proof.Upload {
				return nil, nil, ErrIntegrity
			}
		} else {
			// No upload arrived, or the marker was already compacted before
			// its acknowledgment was lost. The receiver still fences its ID.
			retained = retainedUpload(proof.Upload, s.node, "unreceived")
		}
		release = append(release, retained)
		if len(release) == 256 {
			break
		}
	}
	return release, required, nil
}

type retentionBlob struct {
	scope Scope
	blob  BlobRef
}

func (c retentionCatalog) localProtection(node string) ([]checkpoint.RetainedBlob, map[retentionBlob]bool) {
	var required []checkpoint.RetainedBlob
	pending := map[retentionBlob]bool{}
	for _, claim := range c.receipts {
		if !claim.Released && claim.Receipt.NodeID == node {
			upload := Upload{ID: claim.Receipt.UploadID, Object: claim.Object}
			required = append(required, retainedUpload(upload, node, claim.Receipt.FailureDomain))
		}
	}
	for _, u := range c.uploads {
		if u.State == "pending" && slices.Contains(u.Targets, node) {
			pending[retentionBlob{scope: u.Upload.Object.Scope, blob: u.Upload.Object.Blob}] = true
		}
		if u.State == "prepared" || u.State == "published" {
			for _, receipt := range u.Receipts {
				if _, released := c.releases[receipt.Key()]; receipt.NodeID == node && !released {
					required = append(required, retainedUpload(u.Upload, node, receipt.FailureDomain))
				}
			}
		}
	}
	return required, pending
}
