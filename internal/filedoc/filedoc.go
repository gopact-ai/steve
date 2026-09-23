// Package filedoc keeps a small document in one local file, replaced
// durably. It depends only on the standard library, so a node can keep its
// own state files without linking the hub ledger.
package filedoc

import (
	"fmt"
	"os"
	"path/filepath"
)

// Document is one JSON file kept with the durable-replace discipline:
// temp file, fsync, rename, fsync the directory. It satisfies ledger.Doc.
type Document struct {
	Path string
}

func (f *Document) Load() ([]byte, bool, error) {
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (f *Document) Save(raw []byte) error {
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

func (f *Document) Check() error {
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
		// Best effort: the close failure is the finding; a probe file left
		// behind is harmless.
		_ = os.Remove(name)
		return fmt.Errorf("close check file: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove check file: %w", err)
	}
	return nil
}
