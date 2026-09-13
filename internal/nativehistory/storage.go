package nativehistory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Each node retains source snapshots and evolved execution homes separately.
// Admission bounds both stores without deleting history a user may resume later.
const MaxStoreEntries = 128
const MaxStoreBytes int64 = 8 << 30

var ErrStorageFull = errors.New("native history storage limit reached (128 entries or 8 GiB); archive retained history before importing more")

// CheckStorage must run under LockStorage. Reserve room before copying, then
// recheck the staged files before publication. Abandoned crash stages count too.
func CheckStorage(ctx context.Context, store string, reserveEntries int, reserveBytes int64) error {
	entries, err := os.ReadDir(store)
	if err != nil {
		return err
	}
	count, size := reserveEntries, reserveBytes
	for _, entry := range entries {
		if entry.Name() == ".admission.lock" {
			continue
		}
		count++
		if count > MaxStoreEntries {
			return ErrStorageFull
		}
		err := filepath.WalkDir(filepath.Join(store, entry.Name()), func(path string, d os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				size += info.Size() // WalkDir never follows linked access/skill files.
				if size > MaxStoreBytes {
					return ErrStorageFull
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if size > MaxStoreBytes {
		return ErrStorageFull
	}
	return ctx.Err()
}
