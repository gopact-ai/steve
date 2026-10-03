//go:build !linux && !darwin && !windows

package gitrepo

import (
	"errors"
	"os"
)

func recoveryDirectoryHandleIdentity(*os.File, os.FileInfo) (string, error) {
	return "", errors.New("recovery container identity is unavailable on this platform")
}
