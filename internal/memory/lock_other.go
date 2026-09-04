//go:build windows

package memory

import "sync"

var fallback sync.Mutex

// acquire is process-local where flock is unavailable.
func acquire(string) (func(), error) {
	fallback.Lock()
	return fallback.Unlock, nil
}
