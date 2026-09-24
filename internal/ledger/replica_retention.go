package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrReplayExpired refuses to infer a prior result from an application version.
// The caller must reconcile business facts; it must not retry the mutation with
// a changed expected version.
var ErrReplayExpired = errors.New("ledger: replay receipt expired")

func (l *Ledger) confirmReplicaWrite(id string, version uint64, payload []byte) error {
	// The receipt and physical floor must belong to one read snapshot: a
	// concurrent checkpoint can delete only the latter's proven prefix. The
	// caller holds writerMu, so the snapshot is taken on the writer
	// connection rather than waiting for a pooled read connection.
	err := l.writerRead(func(tx *ReadTx) error {
		var fingerprint string
		err := tx.QueryRow(`SELECT fingerprint FROM replica_commands WHERE id=? AND version=?`, id, version).Scan(&fingerprint)
		if errors.Is(err, sql.ErrNoRows) {
			var floor uint64
			if err := tx.QueryRow(`SELECT replay_floor FROM replica_state WHERE singleton=1`).Scan(&floor); err != nil {
				return err
			}
			if version <= floor {
				return ErrReplayExpired
			}
		}
		if err != nil {
			return fmt.Errorf("proposal acknowledged before local apply: %w", err)
		}
		hash := sha256.Sum256(payload)
		if fingerprint != hex.EncodeToString(hash[:]) {
			return errors.New("proposal acknowledgement does not match the validated batch")
		}
		return nil
	})
	if err == nil || errors.Is(err, ErrReplayExpired) {
		return err
	}
	return l.failReplica(err)
}

// ReplicaCheckpoint is a compacted replica snapshot fixed at one committed
// boundary when it is taken and encoded later: batches applied in between
// neither wait for the encoding nor appear in it. Encode, Persisted and
// Release are called from one goroutine.
type ReplicaCheckpoint struct {
	boundary *replicaBoundary
	floor    uint64
}

// SnapshotReplicaCheckpoint fixes a checkpoint's boundary. Its encoding
// keeps only the replay receipts above floor.
func (l *Ledger) SnapshotReplicaCheckpoint(floor uint64) (*ReplicaCheckpoint, error) {
	if l.replicaWriter {
		return nil, ErrReplicaWriteBypass
	}
	boundary, err := l.replicaBoundary()
	if err != nil {
		return nil, err
	}
	return &ReplicaCheckpoint{boundary: boundary, floor: floor}, nil
}

// Encode copies the boundary into a private database, ends the boundary's
// read transaction, and compacts and frames the copy. It does not change the
// live database.
func (c *ReplicaCheckpoint) Encode() ([]byte, error) {
	path, cleanup, err := c.boundary.copy()
	c.boundary.release()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	err = validateReplicaReceipts(db)
	if err == nil {
		err = pruneReplicaReceipts(db, c.floor)
	}
	if err == nil && c.floor > 0 {
		// DELETE alone leaves old pages in every subsequent database image.
		// Compact the private image, never VACUUM the hot application database.
		_, err = db.Exec(`VACUUM`)
	}
	if err := errors.Join(err, db.Close()); err != nil {
		return nil, err
	}
	return encodeReplicaSnapshot(path, c.boundary.incarnation)
}

// Persisted prunes the live replay receipts at or below the floor. It may be
// called only after the enclosing consensus snapshot sink has durably
// closed; after a restore that followed the boundary it does nothing.
// Failure to prune is retryable maintenance, not failure of that snapshot.
// Business commands and effects are never removed.
func (c *ReplicaCheckpoint) Persisted() error {
	l := c.boundary.l
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if c.boundary.generation != l.snapshotGeneration {
		return nil
	}
	if _, err := l.replicationState(); err != nil {
		return err
	}
	return pruneReplicaReceipts(l.db, c.floor)
}

// Release ends the boundary's read transaction if Encode did not.
func (c *ReplicaCheckpoint) Release() { c.boundary.release() }

func pruneReplicaReceipts(db *sql.DB, floor uint64) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version, prior uint64
	if err := tx.QueryRow(`SELECT version, replay_floor FROM replica_state WHERE singleton=1`).Scan(&version, &prior); err != nil {
		return err
	}
	if floor > version {
		return fmt.Errorf("ledger: replay floor %d exceeds version %d", floor, version)
	}
	if floor <= prior {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM replica_commands WHERE version <= ?`, floor); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE replica_state SET replay_floor=? WHERE singleton=1`, floor); err != nil {
		return err
	}
	return tx.Commit()
}

func validateReplicaReceipts(db *sql.DB) error {
	var version, floor uint64
	if err := db.QueryRow(`SELECT version, replay_floor FROM replica_state WHERE singleton=1`).Scan(&version, &floor); err != nil {
		return err
	}
	var first, latest, count uint64
	if err := db.QueryRow(`SELECT COALESCE(MIN(version), 0), COALESCE(MAX(version), 0), COUNT(*) FROM replica_commands`).Scan(&first, &latest, &count); err != nil {
		return err
	}
	if floor > version || count != version-floor ||
		(count == 0 && (first != 0 || latest != 0)) ||
		(count > 0 && (first != floor+1 || latest != version)) {
		return errors.New("ledger: snapshot replay receipts do not match the retained application suffix")
	}
	return nil
}
