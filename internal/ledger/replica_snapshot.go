package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

type replicaSnapshot struct {
	Format      int    `json:"format"`
	Incarnation uint64 `json:"incarnation"`
	Database    []byte `json:"database"`
}

// backupSource is the side of a driver connection that copies the open
// database into another file through SQLite's online backup API. modernc's
// connection offers it; the driver interface itself does not promise it,
// so the snapshot asks and refuses rather than assumes.
type backupSource interface {
	NewBackup(destination string) (*sqlite.Backup, error)
}

// restoreTarget is the reverse direction on the same connection: replacing
// the open database with another file's content.
type restoreTarget interface {
	NewRestore(source string) (*sqlite.Backup, error)
}

// SnapshotReplica captures an entire SQLite database at one committed
// boundary, including store tables, effects, sequences and replay receipts.
// SQLite's online backup API includes WAL contents and preserves SQL types.
func (l *Ledger) SnapshotReplica() ([]byte, error) {
	if l.replicaWriter {
		return nil, ErrReplicaWriteBypass
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if _, err := l.replicationState(); err != nil {
		return nil, err
	}
	path, cleanup, err := l.replicaTemp(nil)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	conn, err := l.db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var incarnation uint64
	if err := conn.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'incarnation'`).Scan(&incarnation); err != nil {
		return nil, err
	}
	if err := conn.Raw(func(raw any) error {
		provider, ok := raw.(backupSource)
		if !ok {
			return errors.New("ledger: SQLite online backup unavailable")
		}
		backup, err := provider.NewBackup(path)
		if err != nil {
			return err
		}
		_, stepErr := backup.Step(-1)
		return errors.Join(stepErr, backup.Finish())
	}); err != nil {
		return nil, fmt.Errorf("snapshot ledger: %w", err)
	}
	database, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return json.Marshal(replicaSnapshot{Format: 1, Incarnation: incarnation, Database: database})
}

// RestoreReplica durably replaces application state through SQLite's atomic
// backup API. It never replaces an open database file or restores only an
// in-memory connection. A successful restore clears a failed local replica.
func (l *Ledger) RestoreReplica(raw []byte) error {
	if l.replicaWriter {
		return ErrReplicaWriteBypass
	}
	var snapshot replicaSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("decode ledger snapshot: %w", err)
	}
	if snapshot.Format != 1 || snapshot.Incarnation == 0 || len(snapshot.Database) == 0 {
		return errors.New("ledger: invalid replica snapshot")
	}
	path, cleanup, err := l.replicaTemp(snapshot.Database)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := validateReplicaSnapshot(path, snapshot.Incarnation); err != nil {
		return err
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if snapshot.Incarnation < l.Incarnation() {
		return fmt.Errorf("ledger: snapshot incarnation %d predates local %d", snapshot.Incarnation, l.Incarnation())
	}
	if err := (&FileDocument{Path: filepath.Join(l.dir, replicaMarker)}).Save([]byte("1\n")); err != nil {
		return err
	}
	l.mu.Lock()
	l.replicationRequired = true
	l.mu.Unlock()
	intent, err := json.Marshal(replicaRestoreIntent{From: l.Incarnation(), To: snapshot.Incarnation})
	if err != nil {
		return err
	}
	if err := (&FileDocument{Path: filepath.Join(l.dir, replicaRestoreFile)}).Save(intent); err != nil {
		return err
	}
	conn, err := l.db.Conn(context.Background())
	if err != nil {
		return l.failReplica(err)
	}
	err = conn.Raw(func(raw any) error {
		provider, ok := raw.(restoreTarget)
		if !ok {
			return errors.New("ledger: SQLite online restore unavailable")
		}
		backup, err := provider.NewRestore(path)
		if err != nil {
			return err
		}
		_, stepErr := backup.Step(-1)
		return errors.Join(stepErr, backup.Finish())
	})
	// The connection only carried the restore; returning it to the pool
	// cannot change what the restore did.
	_ = conn.Close()
	if err != nil {
		return l.failReplica(fmt.Errorf("restore ledger: %w", err))
	}
	if err := writeIncarnation(filepath.Join(l.dir, incarnationFile), snapshot.Incarnation); err != nil {
		return l.failReplica(err)
	}
	if err := removeReplicaRestoreIntent(l.dir); err != nil {
		return l.failReplica(err)
	}
	l.mu.Lock()
	l.incarnation = snapshot.Incarnation
	l.mu.Unlock()
	l.journal.mu.Lock()
	l.journal.incarnation = snapshot.Incarnation
	l.journal.mu.Unlock()
	if err := l.replaceEffectsJournal(); err != nil {
		return l.failReplica(err)
	}
	l.mu.Lock()
	l.replicaFailure = nil
	l.recovery = false
	l.mu.Unlock()
	return nil
}

const replicaRestoreFile = "replica.restore"

type replicaRestoreIntent struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// A restore spans SQLite and the incarnation file. The durable intent lets
// startup resolve either side of a crash without accepting an unproven
// incarnation rollback. The replicated writer gate remains closed until the
// consensus application has replayed its committed state.
func finishReplicaRestore(dir string, database, incarnation uint64) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, replicaRestoreFile))
	if os.IsNotExist(err) {
		return incarnation, nil
	}
	if err != nil {
		return 0, err
	}
	var intent replicaRestoreIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return 0, fmt.Errorf("decode replica restore intent: %w", err)
	}
	if intent.From == 0 || intent.To < intent.From || (database != intent.From && database != intent.To) || (incarnation != intent.From && incarnation != intent.To) {
		return 0, errors.New("ledger: replica restore intent does not match durable state")
	}
	if _, err := os.Stat(filepath.Join(dir, replicaMarker)); err != nil {
		return 0, fmt.Errorf("restore lacks replication marker: %w", err)
	}
	if err := writeIncarnation(filepath.Join(dir, incarnationFile), database); err != nil {
		return 0, err
	}
	if err := removeReplicaRestoreIntent(dir); err != nil {
		return 0, err
	}
	return database, nil
}

func removeReplicaRestoreIntent(dir string) error {
	if err := os.Remove(filepath.Join(dir, replicaRestoreFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func validateReplicaSnapshot(path string, incarnation uint64) error {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check ledger snapshot: %w", err)
	}
	if integrity != "ok" {
		return errors.New("ledger: snapshot integrity check failed")
	}
	if err := validateReplicaSchema(db); err != nil {
		return err
	}
	actual, err := metaUint(db, "incarnation")
	if err != nil || actual != incarnation {
		return errors.New("ledger: snapshot incarnation mismatch")
	}
	schema, err := metaUint(db, "schema")
	if err != nil || schema != schemaVersion {
		return errors.New("ledger: snapshot schema mismatch")
	}
	version, err := replicaVersion(db)
	if err != nil {
		return err
	}
	var first, latest, count uint64
	if err := db.QueryRow(`SELECT COALESCE(MIN(version), 0), COALESCE(MAX(version), 0), COUNT(*) FROM replica_commands`).Scan(&first, &latest, &count); err != nil {
		return err
	}
	if latest != version || count != version || (version > 0 && first != 1) {
		return errors.New("ledger: snapshot replay receipts do not match application version")
	}
	var effects int64
	return db.QueryRow(`SELECT COUNT(*) FROM effect_entries`).Scan(&effects)
}

func (l *Ledger) replicaTemp(data []byte) (string, func(), error) {
	file, err := os.CreateTemp(l.dir, ".replica-*.db")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	// The staging database and its sidecars are scratch: a leftover only
	// takes space in the ledger directory, is never read, and the ledger
	// itself was not touched.
	cleanup := func() {
		_ = os.Remove(path)
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
		_ = os.Remove(path + "-journal")
	}
	if _, err := file.Write(data); err != nil {
		// The write failure is the finding; the handle is only let go.
		_ = file.Close()
		cleanup()
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
