//go:build linux || darwin

package procgroup

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Kill sends SIGKILL to every process in a group. A group with no process
// left is what a kill wants.
func Kill(group int) error {
	if err := unix.Kill(-group, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// Gone reports whether the kernel finds no process in a group, a zombie
// included. A process this one may not signal, or cannot see in /proc,
// still counts, so an empty process list does not show a group is gone.
func Gone(group int) (bool, error) {
	switch err := unix.Kill(-group, 0); {
	case err == nil, errors.Is(err, unix.EPERM):
		return false, nil
	case errors.Is(err, unix.ESRCH):
		return true, nil
	default:
		return false, err
	}
}
