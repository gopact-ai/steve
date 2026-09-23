package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Doc is one durable document: what the session, task, plan and schedule
// stores keep their whole state in. The ledger's implementation is what a
// Hub runs on; filedoc.Document keeps one in a local file, for tests and for
// state kept outside the ledger.
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

// Import moves a legacy JSON file into the document, once: only when the
// document has never been saved and the file exists. The file is renamed
// with a ".migrated" suffix so it cannot be read as authority again.
func (d *Document) Import(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if _, ok, err := d.Load(); err != nil || ok {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > 0 {
		if err := d.Save(raw); err != nil {
			return false, fmt.Errorf("import %s: %w", path, err)
		}
	}
	if err := os.Rename(path, path+".migrated"); err != nil {
		return false, fmt.Errorf("retire %s: %w", path, err)
	}
	return len(raw) > 0, nil
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
