//go:build unix

package pihome

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockBase serializes preparing the homes under base and their session
// record across every Steve process sharing them: the hub, a node service
// and plugin runtimes of one state directory.
func lockBase(base string) (func(), error) {
	mu := lockFor(base)
	mu.Lock()
	if err := os.MkdirAll(base, 0o700); err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("create pi runtime: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(base, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("open pi runtime lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		mu.Unlock()
		return nil, fmt.Errorf("lock pi runtime: %w", err)
	}
	return func() { _ = f.Close(); mu.Unlock() }, nil
}
