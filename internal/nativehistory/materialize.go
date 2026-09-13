package nativehistory

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Materialize creates a new native home from an immutable history receipt.
// The caller can then install the usual filtered execution configuration.
// Existing destinations and source homes are never modified.
func Materialize(ctx context.Context, store string, ref Reference, dest string) error {
	if !validReferenceID(ref.ID) {
		return errors.New("invalid native import reference")
	}
	dir := filepath.Join(store, ref.ID)
	stored, err := readReference(dir)
	if err != nil {
		return err
	}
	if stored.ID != ref.ID || stored.Harness != ref.Harness || stored.NativeID != ref.NativeID || stored.Digest != ref.Digest || stored.Revision != ref.Revision || stored.SourceHome != ref.SourceHome || stored.SourceWorkdir != ref.SourceWorkdir || !stored.ImportedAt.Equal(ref.ImportedAt) {
		return errors.New("native import receipt does not match the selected snapshot")
	}
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("native runtime destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := os.OpenRoot(filepath.Join(dir, "history"))
	if err != nil {
		return err
	}
	defer root.Close()
	files, err := inventory(ctx, root, Entry{path: "."})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".native-runtime-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	digest, err := copyHistory(ctx, root, stage, files)
	if err != nil {
		return err
	}
	if digest != ref.Digest {
		return errors.New("native history snapshot integrity check failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(dest))
}

func validReferenceID(id string) bool {
	raw, ok := strings.CutPrefix(id, "import_")
	if !ok || len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil && raw == strings.ToLower(raw)
}
