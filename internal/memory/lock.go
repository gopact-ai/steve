//go:build !windows

package memory

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquire takes an exclusive lock on the file, waiting a little for a
// concurrent writer, and returns the release.
func acquire(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK || time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("memory is being written by someone else; try again (%w)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
