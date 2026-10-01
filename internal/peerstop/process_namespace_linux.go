//go:build linux

package peerstop

import (
	"fmt"
	"os"
	"path/filepath"
)

// Command paths describe the installation only in the same filesystem and
// identity view. process calls this both before and after the pidfd is opened.
func checkPeerEnvironment(pid int) error {
	for _, part := range []string{"ns/mnt", "ns/pid", "ns/user", "root"} {
		self, err := os.Stat(filepath.Join("/proc/self", part))
		if err != nil {
			return fmt.Errorf("%w: local process %s cannot be checked", ErrUnproven, part)
		}
		peer, err := os.Stat(filepath.Join(fmt.Sprintf("/proc/%d", pid), part))
		if err != nil || !os.SameFile(self, peer) {
			return fmt.Errorf("%w: peer process %s differs or cannot be checked", ErrUnproven, part)
		}
	}
	return nil
}
