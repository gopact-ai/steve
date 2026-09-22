package contentreplica

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

const (
	uploadKind  = "content-upload"
	receiptKind = "content-receipt"
	releaseKind = "content-release"
)

var (
	ErrReleased   = errors.New("content upload is closed or released")
	ErrReferenced = errors.New("content has live references")
)

type uploadRecord struct {
	Upload   Upload    `json:"upload"`
	State    string    `json:"state"` // pending, prepared, published, aborted
	Targets  []string  `json:"targets"`
	Receipts []Receipt `json:"receipts,omitempty"`
}

type releaseRecord struct {
	Upload    Upload `json:"upload"`
	NodeID    string `json:"node_id"`
	Collected bool   `json:"collected"`
}

type receiptRecord struct {
	Object   Object  `json:"object"`
	Receipt  Receipt `json:"receipt"`
	Released bool    `json:"released"`
}

func loadUpload(tx *ledger.Tx, id string) (uploadRecord, bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, uploadKind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return uploadRecord{}, false, nil
	}
	if err != nil {
		return uploadRecord{}, false, err
	}
	u, err := decodeUpload(id, []byte(raw))
	return u, err == nil, err
}

func decodeUpload(id string, raw []byte) (uploadRecord, error) {
	var u uploadRecord
	if json.Unmarshal(raw, &u) != nil || uploadSequence(id) == 0 || u.Upload.ID != id || validateObject(u.Upload.Object, math.MaxInt64-1) != nil {
		return u, fmt.Errorf("%w: upload %s", ErrIntegrity, id)
	}
	if len(u.Targets) == 0 || len(u.Targets) > 256 || !slices.IsSorted(u.Targets) {
		return u, ErrIntegrity
	}
	for i, node := range u.Targets {
		if !validID(node) || i > 0 && u.Targets[i-1] == node {
			return u, ErrIntegrity
		}
	}
	seen := map[string]bool{}
	for _, r := range u.Receipts {
		if r.UploadID != id || r.ObjectID != u.Upload.Object.ID() || !slices.Contains(u.Targets, r.NodeID) || seen[r.NodeID] || !validID(r.FailureDomain) || r.StoredAt.IsZero() {
			return u, ErrIntegrity
		}
		seen[r.NodeID] = true
	}
	switch u.State {
	case "pending", "prepared", "published", "aborted":
		if (u.State == "prepared" || u.State == "published") && len(u.Receipts) == 0 {
			return u, ErrIntegrity
		}
		return u, nil
	default:
		return u, fmt.Errorf("%w: upload state %s", ErrIntegrity, id)
	}
}

// Reserve establishes a pending publication and its dependency pin in the
// same ledger write boundary used by Retire. Unknown/lost receiver responses
// keep this pin until publication or an explicit AbortUpload.
func Reserve(tx *ledger.Tx, upload Upload, targets []string) error {
	if uploadSequence(upload.ID) == 0 {
		return ErrInvalid
	}
	if err := validateObject(upload.Object, math.MaxInt64-1); err != nil {
		return err
	}
	targets, err := uploadTargets(targets)
	if err != nil {
		return err
	}
	old, found, err := loadUpload(tx, upload.ID)
	if err != nil {
		return err
	}
	if found {
		if old.Upload != upload || !slices.Equal(old.Targets, targets) {
			return ErrIntegrity
		}
		if old.State != "pending" {
			return ErrReleased
		}
		return nil
	}
	high, err := readUploadClock(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return err
	}
	if uploadSequence(upload.ID) <= high {
		return ErrReleased
	}
	if upload.Object.Base != "" {
		chain, err := closure(upload.Object.Base, func(id string) (Manifest, bool, error) { return lookupTx(tx, id) })
		if err != nil {
			return err
		}
		base := chain[len(chain)-1].Object
		if len(chain) >= MaxBundleDepth || base.Scope != upload.Object.Scope || base.Kind != GitBundle || base.Key == upload.Object.Key {
			return ErrIntegrity
		}
	}
	if err := tx.PutBinding(uploadClockKind, "sequence", uploadClock{High: uploadSequence(upload.ID)}); err != nil {
		return err
	}
	return tx.PutBinding(uploadKind, upload.ID, uploadRecord{Upload: upload, State: "pending", Targets: targets})
}

func uploadTargets(targets []string) ([]string, error) {
	targets = slices.Clone(targets)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	if len(targets) == 0 || len(targets) > 256 {
		return nil, ErrInvalid
	}
	for _, node := range targets {
		if !validID(node) {
			return nil, ErrInvalid
		}
	}
	return targets, nil
}

// sealUpload fixes the exact receipts available to publication. A target
// whose reply was lost is explicitly released, not silently forgotten: no
// delayed receipt from it can be added to this attempt after this transaction.
func sealUpload(tx *ledger.Tx, id string, receipts []Receipt) error {
	u, ok, err := loadUpload(tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIncomplete
	}
	if u.State != "pending" {
		return ErrReleased
	}
	u.State, u.Receipts = "prepared", slices.Clone(receipts)
	raw, _ := json.Marshal(u)
	if _, err := decodeUpload(id, raw); err != nil {
		return err
	}
	for _, node := range u.Targets {
		if !slices.ContainsFunc(receipts, func(r Receipt) bool { return r.NodeID == node }) {
			if err := releaseReceipt(tx, u.Upload, node); err != nil {
				return err
			}
		}
	}
	return tx.PutBinding(uploadKind, id, u)
}

func releaseReceipt(tx *ledger.Tx, upload Upload, node string) error {
	r := Receipt{UploadID: upload.ID, ObjectID: upload.Object.ID(), NodeID: node}
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, releaseKind, r.Key()).Scan(&raw)
	if err == nil {
		var existing releaseRecord
		if json.Unmarshal([]byte(raw), &existing) != nil || existing.Upload != upload || existing.NodeID != node {
			return ErrIntegrity
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return tx.PutBinding(releaseKind, r.Key(), releaseRecord{Upload: upload, NodeID: node})
}

// AbortUpload explicitly closes an unpublished attempt. It is not inferred
// from age or a missing owner. Receivers may release only exact receipt keys
// whose durable descriptor matches this aborted upload. A published attempt
// must instead use reference-aware retirement or superseded-receipt release.
func AbortUpload(tx *ledger.Tx, id string) error {
	u, ok, err := loadUpload(tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIncomplete
	}
	if u.State == "published" {
		return ErrReferenced
	}
	for _, node := range u.Targets {
		if err := releaseReceipt(tx, u.Upload, node); err != nil {
			return err
		}
	}
	u.State = "aborted"
	return tx.PutBinding(uploadKind, id, u)
}

func equalReceipt(a, b Receipt) bool {
	return a.UploadID == b.UploadID && a.ObjectID == b.ObjectID && a.NodeID == b.NodeID &&
		a.FailureDomain == b.FailureDomain && a.StoredAt.Equal(b.StoredAt)
}

// publishReceipts preserves every exact confirmation, even when the current
// manifest replaces an older receipt for the same physical node.
func publishReceipts(tx *ledger.Tx, m Manifest) error {
	uploads := map[string]uploadRecord{}
	for _, r := range m.Receipts {
		u, ok, err := loadUpload(tx, r.UploadID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: upload %s", ErrIncomplete, r.UploadID)
		}
		if u.Upload.Object != m.Object {
			return ErrIntegrity
		}
		if u.State != "prepared" && u.State != "published" {
			return ErrReleased
		}
		if !slices.ContainsFunc(u.Receipts, func(issued Receipt) bool { return equalReceipt(issued, r) }) {
			return ErrReleased
		}
		var raw string
		err = tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, receiptKind, r.Key()).Scan(&raw)
		if err == nil {
			var old receiptRecord
			if json.Unmarshal([]byte(raw), &old) != nil || old.Object != m.Object || !equalReceipt(old.Receipt, r) {
				return ErrIntegrity
			}
			if old.Released {
				return ErrReleased
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if u.State != "prepared" {
			// A delayed receipt may not enlarge a closed publication.
			return ErrReleased
		}
		uploads[r.UploadID] = u
	}
	for _, r := range m.Receipts {
		if err := tx.PutBinding(receiptKind, r.Key(), receiptRecord{Object: m.Object, Receipt: r}); err != nil {
			return err
		}
	}
	for id, u := range uploads {
		u.State = "published"
		if err := tx.PutBinding(uploadKind, id, u); err != nil {
			return err
		}
	}
	return nil
}
