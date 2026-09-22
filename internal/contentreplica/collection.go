package contentreplica

import "github.com/gopact-ai/steve/internal/ledger"

// ConfirmCollection records exact receiver confirmations in the same write
// boundary as history compaction. The caller must authenticate node as the
// receiver (never accept a user-supplied list as deletion authorization).
// Missing keys are already compacted retries; they authorize no new action.
func ConfirmCollection(tx *ledger.Tx, node string, keys []string) error {
	if !validID(node) || len(keys) > 256 {
		return ErrInvalid
	}
	c, err := readRetentionCatalog(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !digest(key, 64) {
			return ErrInvalid
		}
		r, found := c.releases[key]
		if !found {
			continue
		}
		if r.NodeID != node {
			return ErrIntegrity
		}
		r.Collected = true
		c.releases[key] = r
		if err := tx.PutBinding(releaseKind, key, r); err != nil {
			return err
		}
	}
	for id, u := range c.uploads {
		if !c.uploadCollected(u) {
			continue
		}
		for _, node := range u.Targets {
			key := (Receipt{UploadID: id, ObjectID: u.Upload.Object.ID(), NodeID: node}).Key()
			for _, kind := range []string{receiptKind, releaseKind} {
				if _, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, kind, key); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, uploadKind, id); err != nil {
			return err
		}
	}
	return nil
}

func (c retentionCatalog) uploadCollected(u uploadRecord) bool {
	if u.State != "aborted" && u.State != "published" {
		return false
	}
	for _, node := range u.Targets {
		key := (Receipt{UploadID: u.Upload.ID, ObjectID: u.Upload.Object.ID(), NodeID: node}).Key()
		r, ok := c.releases[key]
		if !ok || !r.Collected {
			return false
		}
		if claim, ok := c.receipts[key]; ok && !claim.Released {
			return false
		}
	}
	return true
}
