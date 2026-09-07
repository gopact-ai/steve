//go:build unix

package transfer

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// syncTransferPath flushes a complete installed tree before its metadata can
// activate. Each child is opened relative to its pinned parent without following
// symlinks; links are persisted by syncing their containing directory.
func syncTransferPath(path string, syncFile func(*os.File) error) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Resolve the caller's directory aliases (for example macOS /var) once.
	// The imported root itself must be a real file or directory.
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return err
	}
	path = filepath.Join(parent, filepath.Base(abs))
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open imported path %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	err = syncTransferTree(f, syncFile)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// MkdirAll can have created several ancestors. Flush their entries from
	// the innermost parent outward, including the entry naming the state dir.
	for dir := parent; ; dir = filepath.Dir(dir) {
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), dir)
		err = syncFile(f)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("sync imported directory %s: %w", dir, err)
		}
		if closeErr != nil {
			return closeErr
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func syncTransferTree(f *os.File, syncFile func(*os.File) error) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := f.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			fd, err := unix.Openat(int(f.Fd()), entry.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("open imported entry %s: %w", filepath.Join(f.Name(), entry.Name()), err)
			}
			child := os.NewFile(uintptr(fd), filepath.Join(f.Name(), entry.Name()))
			err = syncTransferTree(child, syncFile)
			closeErr := child.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported imported entry %s", f.Name())
	}
	if err := syncFile(f); err != nil {
		return fmt.Errorf("sync imported entry %s: %w", f.Name(), err)
	}
	return nil
}
