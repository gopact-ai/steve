// Package fsx publishes small files so that a crash leaves either the old
// content or the complete new content, never a torn file.
//
// Every published file is private (0600). Its content is synced before it
// becomes visible under its final name.
package fsx

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data and makes the replacement durable: it is
// ReplaceFile followed by SyncDir of path's directory.
func WriteFile(path string, data []byte) error {
	if err := ReplaceFile(path, data); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// ReplaceFile atomically replaces path with data. Readers see the old file or
// the new one. The rename itself is not yet durable; callers that must tell a
// failed replacement from a replacement whose directory sync failed call
// SyncDir themselves, everyone else uses WriteFile.
func ReplaceFile(path string, data []byte) error {
	temp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		// Best effort: the rename failure is what the caller must see.
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// CreateFile publishes data at path only if nothing is there yet; otherwise
// it returns an error matching fs.ErrExist and leaves the existing file
// untouched. Concurrent creators agree on exactly one winner.
func CreateFile(path string, data []byte) error {
	temp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	// The temporary name is only a staging link; the published name keeps
	// the inode whether or not its removal succeeds.
	defer os.Remove(temp)
	if err := os.Link(temp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir flushes dir's entries, so a file created, renamed or removed in it
// survives a crash. A rename alone is not durable on every filesystem.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// writeTemp stores data, synced, in a private temporary file beside path and
// returns its name. Nothing is left behind on failure.
func writeTemp(path string, data []byte) (string, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	err = temp.Chmod(0o600)
	if err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// Best effort: the write failure is what the caller must see.
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
