package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const initializedContents = "1\n"

// A marker is complete only when it has the exact small format. Unknown
// contents are not a repository version to migrate or a reason to reinitialize.
func initialized(dir string) (bool, error) {
	path := filepath.Join(dir, initializedMarker)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(initializedContents)) {
		return false, fmt.Errorf("repository initialization marker is incomplete or unrecognized: %s", dir)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return false, errors.Join(statErr, file.Close(), errors.New("repository initialization marker changed while opening"))
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, int64(len(initializedContents)+1)))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return false, err
	}
	if string(contents) != initializedContents {
		return false, fmt.Errorf("repository initialization marker is incomplete or unrecognized: %s", dir)
	}
	return true, nil
}

// A closed private file is published without replacing any existing path.
// Link is the no-clobber publication primitive; a filesystem without hard
// links returns an error instead of falling back to an overwriting rename.
func writeInitializationMarker(ctx context.Context, dir string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".steve-initialized-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(file.Name())) }()
	return publishInitializationMarker(ctx, dir, file.Name(), file)
}

func publishInitializationMarker(ctx context.Context, dir, staging string, file io.WriteCloser) error {
	written, writeErr := io.WriteString(file, initializedContents)
	if writeErr == nil && written != len(initializedContents) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(staging, filepath.Join(dir, initializedMarker)); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("publish repository initialization marker without replacing it: %w", err)
		}
		ready, inspectErr := initialized(dir)
		if inspectErr != nil || ready {
			return inspectErr
		}
		return errors.New("repository initialization marker disappeared")
	}
	return nil
}
