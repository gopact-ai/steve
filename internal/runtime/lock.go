//go:build unix

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireLock takes an exclusive advisory lock scoped to the state
// directory, so a second gateway over the same state cannot start. Two
// gateways on one Feishu app split the event stream between them — half the
// traffic silently lands on the other process — so this must be refused
// structurally, not by operator discipline. The returned release func
// unlocks; the lock also dies with the process, which is the point.
func AcquireLock(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(stateDir, "gateway.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open gateway lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another gateway already serves %s (lock %s is held)", stateDir, path)
	}
	// The pid is a courtesy to whoever finds the lock held; the flock is
	// what excludes, so a failed write costs only that hint.
	_ = file.Truncate(0)
	_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	_ = file.Sync()
	return func() {
		// Release also runs mid-process — node adoption, project export and
		// import, and a cluster peer each hold the lock for one bounded
		// operation and expect the next AcquireLock over the same directory
		// to succeed. Both errors are still dropped, and release keeps its
		// bare func() shape, because neither can leave the directory locked:
		// the flock lives on this open file description, which the kernel
		// drops with the descriptor whichever way close answers, so the
		// unlock only surrenders it a moment earlier.
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
