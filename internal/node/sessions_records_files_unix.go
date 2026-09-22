//go:build unix

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// The private directory excludes other users; the descriptor lock excludes
// cooperating owners. SQLite opens by path, so keep the verified DB descriptor
// and recheck its identity before handing that path to the driver.
type sessionRecordFiles struct {
	path    string
	db      *os.File
	lock    *os.File
	created bool
	once    sync.Once
	err     error
}

func privateSessionFile(path string, flags int) (*os.File, error) {
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open node records file %s: %w", filepath.Base(path), err)
	}
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
			err = errors.New("node records require an owner-private regular file with one link")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func prepareSessionRecordFiles(path string) (_ *sessionRecordFiles, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	directory, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("node records require an owner-private directory")
	}
	files := &sessionRecordFiles{path: path}
	defer func() {
		if err != nil {
			_ = files.close()
		}
	}()
	files.lock, err = privateSessionFile(path+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(files.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("node records already owned: %w", err)
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(path)
	for _, entry := range entries {
		switch entry.Name() {
		case base, base + ".lock", base + "-wal", base + "-shm", base + "-journal":
			file, err := privateSessionFile(filepath.Join(dir, entry.Name()), os.O_RDWR)
			if err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
		default:
			if strings.HasSuffix(entry.Name(), ".json") {
				return nil, errors.New("legacy node session JSON is unsupported; explicit state reset is required")
			}
			return nil, fmt.Errorf("unknown node records entry %q", entry.Name())
		}
	}
	// Never let SQLite create the main file with its default creation mode.
	files.db, err = privateSessionFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err == nil {
		files.created = true
	} else if errors.Is(err, os.ErrExist) {
		files.db, err = privateSessionFile(path, os.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(dir)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("node records directory changed during open")
	}
	if err := files.validateIdentity(); err != nil {
		return nil, err
	}
	if files.created {
		if err := files.db.Sync(); err != nil {
			return nil, err
		}
		if err := directory.Sync(); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (f *sessionRecordFiles) validateIdentity() error {
	current, err := privateSessionFile(f.path, os.O_RDWR)
	if err != nil {
		return err
	}
	defer current.Close()
	before, err := f.db.Stat()
	if err != nil {
		return err
	}
	after, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("node records database changed during open")
	}
	return nil
}

func (f *sessionRecordFiles) close() error {
	f.once.Do(func() {
		if f.db != nil {
			f.err = f.db.Close()
		}
		if f.lock != nil {
			f.err = errors.Join(f.err, syscall.Flock(int(f.lock.Fd()), syscall.LOCK_UN), f.lock.Close())
		}
	})
	return f.err
}
