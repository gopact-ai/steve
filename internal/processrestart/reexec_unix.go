//go:build unix

// Package processrestart replaces a stopped service with its current executable.
// Callers own admission, durable receipts and graceful shutdown before this call.
package processrestart

import (
	"os"
	"syscall"
)

func Supported() bool { return true }

func ReexecCurrent() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(executable, os.Args, os.Environ())
}
