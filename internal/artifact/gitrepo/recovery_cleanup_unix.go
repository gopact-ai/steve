//go:build linux || darwin

package gitrepo

import (
	"fmt"
	"os"
	"syscall"
)

func recoveryDirectoryHandleIdentity(_ *os.File, info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrPreparedWorkspaceChanged
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}
