package ledger

import (
	"context"
	"database/sql"
	"errors"
)

// Reader is the bounded row-read port shared by mutation and read-only
// transactions. Domain admission checks can use either without opening a
// second database snapshot or gaining write authority.
type Reader interface {
	QueryRow(query string, args ...any) *Row
}

// ReadTx exposes only reads within one committed SQLite snapshot.
type ReadTx struct {
	ctx context.Context
	tx  *sql.Tx
}

// Read does not acquire coordinator authority or propose replicated writes.
// A caller needing a linearizable follower read must first wait for the
// required replica version; every query in fn then shares one local snapshot.
// That snapshot is of committed state only; a mutation in progress reads
// through its Tx instead.
func (l *Ledger) Read(ctx context.Context, fn func(*ReadTx) error) error {
	tx, err := l.reads.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&ReadTx{ctx: ctx, tx: tx})
}

// writerRead is Read on the writer connection, for commit and apply paths
// that hold writerMu or applyMu and must not wait for the read pool.
func (l *Ledger) writerRead(fn func(*ReadTx) error) error {
	ctx := context.Background()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&ReadTx{ctx: ctx, tx: tx})
}

func (t *ReadTx) QueryRow(query string, args ...any) *Row {
	if !readOnlyQuery(query) {
		return &Row{err: ErrReplicaWriteBypass}
	}
	return &Row{row: t.tx.QueryRowContext(t.ctx, query, args...)}
}

func (t *ReadTx) Query(query string, args ...any) (*sql.Rows, error) {
	if !readOnlyQuery(query) {
		return nil, ErrReplicaWriteBypass
	}
	return t.tx.QueryContext(t.ctx, query, args...)
}

// LeaseOf reads the exact current lease tuple in the same snapshot as its
// execution and task records.
func (t *ReadTx) LeaseOf(key string) (Lease, bool, error) {
	var lease Lease
	var expires string
	err := t.QueryRow(`SELECT incarnation, epoch, holder, expires_at FROM leases WHERE resource_key = ?`, key).
		Scan(&lease.Incarnation, &lease.Epoch, &lease.Holder, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	lease.Key = key
	if lease.ExpiresAt, err = stamp(expires); err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}
