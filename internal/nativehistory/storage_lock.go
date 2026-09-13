//go:build unix

package nativehistory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const StorageSupported = true

// LockStorage serializes admission and publication across node processes; a
// crash releases the descriptor, while its unpublished stage stays accounted.
func LockStorage(ctx context.Context, store string) (func(), error) {
	if err := os.MkdirAll(store, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(store, ".admission.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
