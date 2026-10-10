//go:build darwin

package acphost

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Darwin cannot exec /dev/fd/N. Pin the opened self image at a private ordinary
// path instead; never let a replacement at os.Executable's path become a helper.
// Linux keeps its descriptor execution path and needs no temporary image.
func openTerminalExecutable() (*os.File, string, func(), error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, "", nil, err
	}
	self, err := os.Open(executable)
	if err != nil {
		return nil, "", nil, err
	}
	path, release, err := pinTerminalExecutable(self, executable)
	if err != nil {
		self.Close()
		return nil, "", nil, err
	}
	return self, path, release, nil
}

func pinTerminalExecutable(self *os.File, executable string) (path string, release func(), result error) {
	original, err := self.Stat()
	if err != nil || !original.Mode().IsRegular() || original.Mode().Perm()&0111 == 0 {
		return "", nil, errors.New("terminal helper image is not a regular executable")
	}
	dir, err := os.MkdirTemp("/tmp", "steve-terminal-")
	if err != nil {
		return "", nil, err
	}
	pinned := filepath.Join(dir, "helper")
	path = pinned
	release = func() { _ = os.Remove(pinned); _ = os.Remove(dir) }
	defer func() {
		if result != nil {
			release()
		}
	}()
	if err := os.Link(executable, path); err == nil {
		linked, err := os.Lstat(path)
		if err != nil || !linked.Mode().IsRegular() || !os.SameFile(original, linked) {
			return "", release, errors.New("terminal helper image changed before pinning")
		}
	} else if errors.Is(err, syscall.EXDEV) {
		// An installed binary can live on another volume. Copy from the captured
		// FD, not its replaceable source pathname, into the same private directory.
		if err := copyTerminalExecutable(self, path); err != nil {
			return "", release, err
		}
	} else {
		return "", release, err
	}
	return path, release, nil
}

func copyTerminalExecutable(self *os.File, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, self)
	return errors.Join(copyErr, file.Close())
}
