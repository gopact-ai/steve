//go:build windows

package memory

import "sync"

var fallback sync.Map

// acquire is process-local where flock is unavailable.
func acquire(path string) (func(), error) {
	// Receipt and markdown locks can be held together, as on Unix.
	value, _ := fallback.LoadOrStore(path, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}
