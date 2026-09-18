//go:build unix

// Package processrestart replaces a stopped service with its current executable.
// Callers own admission, durable receipts and graceful shutdown before this call.
package processrestart

import (
	"os"
	"syscall"
)

func Supported() bool { return true }

func ReexecCurrent() error { return Reexec("") }

// Reexec continues this process as program, keeping its identity, arguments
// and environment. An empty program means the executable this process was
// started from, which is what a service that restarts for its own reasons
// wants; a launcher that installed a new build names it instead.
func Reexec(program string) error {
	if program == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		program = executable
	}
	return syscall.Exec(program, os.Args, os.Environ())
}
