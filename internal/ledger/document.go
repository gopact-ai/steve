package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Doc is one durable document: what the session, plan and schedule stores
// keep their whole state in. The ledger's implementation is what a Hub runs
// on; filedoc.Document keeps one in a local file, for state kept outside the
// ledger.
type Doc interface {
	// Load returns the document, or ok=false if it has never been saved.
	Load() ([]byte, bool, error)
	// Save replaces the document durably: it is either all there or the
	// previous version is.
	Save([]byte) error
	// Check proves the document can be written before anything depends
	// on it.
	Check() error
}

// Document is a Doc kept in the ledger's bindings table.
type Document struct {
	l    *Ledger
	kind string
}

const documentKind = "document"

// Document returns the ledger-backed document of a kind ("state", "tasks"…).
func (l *Ledger) Document(kind string) *Document {
	return &Document{l: l, kind: kind}
}

func (d *Document) Load() ([]byte, bool, error) {
	var data string
	err := d.l.db.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, documentKind, d.kind).Scan(&data)
	if errors.Is(err, errNoRows()) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(data), true, nil
}

func (d *Document) Save(raw []byte) error {
	_, err := d.l.execWrite(context.Background(), `INSERT INTO bindings(kind, id, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		documentKind, d.kind, string(raw), d.l.now().UTC().Format(rfc3339nano))
	return err
}

func (d *Document) Check() error {
	tx, err := d.l.beginWrite(context.Background())
	if err != nil {
		return fmt.Errorf("ledger not writable: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), `UPDATE bindings SET data = data WHERE 0`); err != nil {
		tx.Rollback()
		return fmt.Errorf("ledger not writable: %w", err)
	}
	return tx.Rollback()
}

// LoadDocument reads a document under the caller's existing transaction.
func (t *Tx) LoadDocument(kind string) ([]byte, bool, error) {
	var data string
	err := t.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, documentKind, kind).Scan(&data)
	if errors.Is(err, errNoRows()) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(data), true, nil
}
