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
