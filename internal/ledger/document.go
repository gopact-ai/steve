package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Doc is one durable document: what the session, task, plan and schedule
// stores keep their whole state in. Two implementations exist. The ledger's
// is the authority; the file one survives for tests and for reading the
// JSON files a pre-ledger deployment left behind.
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
	_, err := d.l.db.Exec(`INSERT INTO bindings(kind, id, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		documentKind, d.kind, string(raw), d.l.now().UTC().Format(rfc3339nano))
	return err
}

func (d *Document) Check() error {
	tx, err := d.l.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("ledger not writable: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('check', '1') ON CONFLICT(key) DO UPDATE SET value = '1'`); err != nil {
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

// FileDocument is a Doc kept in one JSON file with the durable-replace
// discipline: temp file, fsync, rename, fsync the directory.
type FileDocument struct {
	Path string
}

func (f *FileDocument) Load() ([]byte, bool, error) {
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (f *FileDocument) Save(raw []byte) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(f.Path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure temp file: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(name, f.Path); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func (f *FileDocument) Check() error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".check-*")
	if err != nil {
		return fmt.Errorf("check directory: %w", err)
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close check file: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove check file: %w", err)
	}
	return nil
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
