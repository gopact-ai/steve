// Package filedoc keeps a small document in one local file, replaced
// durably. It depends only on the standard library and internal/fsx, so a node
// can keep its own state files without linking the hub ledger.
package filedoc

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/fsx"
)

// Document is one file kept with the durable-replace discipline:
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
	if err := fsx.WriteFile(f.Path, raw); err != nil {
		return fmt.Errorf("replace: %w", err)
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
