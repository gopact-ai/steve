package ledger

import (
	"context"
	"database/sql"
)

// Database is limited diagnostic access to the ledger file. It intentionally
// exposes no raw connection, prepared statement or transaction that could
// survive AttachReplication and later bypass consensus.
type Database struct{ l *Ledger }

func (d *Database) Exec(query string, args ...any) (sql.Result, error) {
	return d.ExecContext(context.Background(), query, args...)
}

func (d *Database) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	d.l.writerMu.Lock()
	defer d.l.writerMu.Unlock()
	r, err := d.l.writeState()
	if err != nil {
		return nil, err
	}
	if r != nil {
		return nil, ErrReplicaWriteBypass
	}
	return d.l.db.ExecContext(ctx, query, args...)
}

func (d *Database) QueryRow(query string, args ...any) *Row {
	return d.QueryRowContext(context.Background(), query, args...)
}

func (d *Database) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if !readOnlyQuery(query) {
		return &Row{err: ErrReplicaWriteBypass}
	}
	return &Row{row: d.l.db.QueryRowContext(ctx, query, args...)}
}

func (d *Database) Query(query string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), query, args...)
}

func (d *Database) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !readOnlyQuery(query) {
		return nil, ErrReplicaWriteBypass
	}
	return d.l.db.QueryContext(ctx, query, args...)
}
