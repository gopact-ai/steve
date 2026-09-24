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
	"sync"

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

// SnapshotReplica captures an entire SQLite database at one committed
// boundary, including store tables, effects, sequences and replay receipts.
// SQLite's online backup API includes WAL contents and preserves SQL types.
func (l *Ledger) SnapshotReplica() ([]byte, error) {
	if l.replicaWriter {
		return nil, ErrReplicaWriteBypass
	}
	boundary, err := l.replicaBoundary()
	if err != nil {
		return nil, err
	}
	path, cleanup, err := boundary.copy()
	boundary.release()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// The private backup is detached from the live database. Preparing its
	// read schema and framing it need not hold the apply lock; the read
	// transaction the backup copied defines the committed boundary.
	return encodeReplicaSnapshot(path, boundary.incarnation)
}

// replicaBoundary is the database as of one committed boundary: a read
// transaction on a pooled read connection, begun under applyMu so that it
// follows every applied batch and precedes the next. Copying it runs from
// that transaction's WAL snapshot without applyMu or the writer connection,
// so batches applied and writes made after the boundary neither wait for
// the copy nor appear in it.
type replicaBoundary struct {
	l                       *Ledger
	conn                    *sql.Conn
	incarnation, generation uint64
	once                    sync.Once
}

// replicaBoundary opens a boundary. The read connection is taken before
// applyMu: a path holding applyMu must not wait for the read pool.
func (l *Ledger) replicaBoundary() (*replicaBoundary, error) {
	ctx := context.Background()
	conn, err := l.reads.Conn(ctx)
	if err != nil {
		return nil, err
	}
	b := &replicaBoundary{l: l, conn: conn}
	if err := b.begin(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return b, nil
}

// begin opens the read transaction under applyMu and reads the incarnation
// in it, which fixes the transaction's snapshot.
func (b *replicaBoundary) begin(ctx context.Context) error {
	b.l.applyMu.Lock()
	defer b.l.applyMu.Unlock()
	if _, err := b.l.replicationState(); err != nil {
		return err
	}
	if _, err := b.conn.ExecContext(ctx, `BEGIN`); err != nil {
		return err
	}
	if err := b.conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'incarnation'`).Scan(&b.incarnation); err != nil {
		_, _ = b.conn.ExecContext(ctx, `ROLLBACK`)
		return err
	}
	b.generation = b.l.snapshotGeneration
	return nil
}

// copy backs the boundary's snapshot up into a private file.
func (b *replicaBoundary) copy() (string, func(), error) {
	path, cleanup, err := b.l.replicaTemp(nil)
	if err != nil {
		return "", nil, err
	}
	if err := b.conn.Raw(func(raw any) error {
		provider, ok := raw.(backupSource)
		if !ok {
			return errors.New("ledger: SQLite online backup unavailable")
		}
		backup, err := provider.NewBackup(path)
		if err != nil {
			return err
		}
		if b.l.backupStarted != nil {
			b.l.backupStarted()
		}
		// With a read transaction already open on the source connection,
		// the backup reads through it rather than beginning its own, and
		// one step copies every page before the transaction ends.
		_, stepErr := backup.Step(-1)
		return errors.Join(stepErr, backup.Finish())
	}); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("snapshot ledger: %w", err)
	}
	return path, cleanup, nil
}

// release ends the read transaction and returns its connection to the pool.
// A failed rollback leaves the connection unusable, so it is discarded.
func (b *replicaBoundary) release() {
	b.once.Do(func() {
		if _, err := b.conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			_ = b.conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = b.conn.Close()
	})
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
