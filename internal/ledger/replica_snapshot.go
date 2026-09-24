package ledger

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/filedoc"
	"modernc.org/sqlite"
)

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

// replicaBackupStarted, when set, runs as a snapshot's backup begins
// copying pages.
var replicaBackupStarted func()

// SnapshotReplica captures an entire SQLite database at one committed
// boundary, including store tables, effects, sequences and replay receipts.
// SQLite's online backup API includes WAL contents and preserves SQL types.
func (l *Ledger) SnapshotReplica() ([]byte, error) {
	if l.replicaWriter {
		return nil, ErrReplicaWriteBypass
	}
	path, incarnation, _, cleanup, err := l.captureReplicaDatabase()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// The private backup is detached from the live database. Preparing its
	// read schema and framing it need not hold the apply lock; the read
	// transaction the backup copied defines the committed boundary.
	return encodeReplicaSnapshot(path, incarnation)
}

// captureReplicaDatabase copies the database as of one committed boundary
// into a private file. The boundary is a read transaction on a pooled read
// connection, begun under applyMu so that it follows every applied batch
// and precedes the next; the copy then runs from that transaction's WAL
// snapshot with applyMu released, so neither applies nor local writes wait
// for it. The read connection is taken before applyMu: a path holding
// applyMu must not wait for the read pool.
func (l *Ledger) captureReplicaDatabase() (string, uint64, uint64, func(), error) {
	ctx := context.Background()
	path, cleanup, err := l.replicaTemp(nil)
	if err != nil {
		return "", 0, 0, nil, err
	}
	complete := false
	defer func() {
		if !complete {
			cleanup()
		}
	}()
	conn, err := l.reads.Conn(ctx)
	if err != nil {
		return "", 0, 0, nil, err
	}
	defer conn.Close()
	incarnation, generation, err := l.beginReplicaBoundary(ctx, conn)
	if err != nil {
		return "", 0, 0, nil, err
	}
	// Ending the transaction returns the connection to the pool without
	// one open; a failed rollback leaves it unusable, so it is discarded.
	defer func() {
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := conn.Raw(func(raw any) error {
		provider, ok := raw.(backupSource)
		if !ok {
			return errors.New("ledger: SQLite online backup unavailable")
		}
		backup, err := provider.NewBackup(path)
		if err != nil {
			return err
		}
		if replicaBackupStarted != nil {
			replicaBackupStarted()
		}
		// The source connection's open read transaction is the snapshot the
		// backup copies; writers on other connections do not restart it.
		_, stepErr := backup.Step(-1)
		return errors.Join(stepErr, backup.Finish())
	}); err != nil {
		return "", 0, 0, nil, fmt.Errorf("snapshot ledger: %w", err)
	}
	complete = true
	return path, incarnation, generation, cleanup, nil
}

// beginReplicaBoundary opens a read transaction on conn under applyMu and
// reads the incarnation in it, which fixes the transaction's snapshot.
func (l *Ledger) beginReplicaBoundary(ctx context.Context, conn *sql.Conn) (uint64, uint64, error) {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if _, err := l.replicationState(); err != nil {
		return 0, 0, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return 0, 0, err
	}
	var incarnation uint64
	if err := conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'incarnation'`).Scan(&incarnation); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return 0, 0, err
	}
	return incarnation, l.snapshotGeneration, nil
}

// RestoreReplica durably replaces application state through SQLite's atomic
// backup API. It never replaces an open database file or restores only an
// in-memory connection. A successful restore clears a failed local replica.
// The replacement commits as one write: a read transaction already open
// keeps its snapshot of the old state, and every later read sees the new
// one. RestoreGeneration brackets the replacement for readers that cache.
func (l *Ledger) RestoreReplica(raw []byte) error {
	if l.replicaWriter {
		return ErrReplicaWriteBypass
	}
	incarnation, database, err := decodeReplicaSnapshot(raw)
	if err != nil {
		return err
	}
	path, cleanup, err := l.replicaTemp(database)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := validateReplicaSnapshot(path, incarnation); err != nil {
		return err
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.restoreGeneration.Add(1)
	defer l.restoreGeneration.Add(1)
	// A captured checkpoint can finish persisting concurrently with Restore.
	// Invalidate its physical-pruning callback before replacing any live facts.
	l.snapshotGeneration++
	if incarnation < l.Incarnation() {
		return fmt.Errorf("ledger: snapshot incarnation %d predates local %d", incarnation, l.Incarnation())
	}
	if err := (&filedoc.Document{Path: filepath.Join(l.dir, replicaMarker)}).Save([]byte("1\n")); err != nil {
		return err
	}
	l.mu.Lock()
	l.replicationRequired = true
	l.mu.Unlock()
	intent, err := json.Marshal(replicaRestoreIntent{From: l.Incarnation(), To: incarnation})
	if err != nil {
		return err
	}
	if err := (&filedoc.Document{Path: filepath.Join(l.dir, replicaRestoreFile)}).Save(intent); err != nil {
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
	if err := writeIncarnation(filepath.Join(l.dir, incarnationFile), incarnation); err != nil {
		return l.failReplica(err)
	}
	if err := removeReplicaRestoreIntent(l.dir); err != nil {
		return l.failReplica(err)
	}
	l.mu.Lock()
	l.incarnation = incarnation
	l.mu.Unlock()
	l.journal.mu.Lock()
	l.journal.incarnation = incarnation
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
	// This is a private staging database, not the live replica. Rebuild only
	// derived read indexes before accepting it, including on writer handles.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := validateReplicaSchema(db); err != nil {
		return err
	}
	if err := ensureReadIndexes(db); err != nil {
		return err
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check ledger snapshot: %w", err)
	}
	if integrity != "ok" {
		return errors.New("ledger: snapshot integrity check failed")
	}
	actual, err := metaUint(db, "incarnation")
	if err != nil || actual != incarnation {
		return errors.New("ledger: snapshot incarnation mismatch")
	}
	schema, err := metaUint(db, "schema")
	if err != nil || schema != schemaVersion {
		return errors.New("ledger: snapshot schema mismatch")
	}
	if err := validateReplicaReceipts(db); err != nil {
		return err
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
