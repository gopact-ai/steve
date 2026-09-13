//go:build !unix

package runtime

// LockSupported reports whether AcquireLock enforces process exclusion.
const LockSupported = false

// AcquireLock is a no-op where flock is unavailable; the singleton guarantee
// is unix-only for now.
func AcquireLock(string) (func(), error) { return func() {}, nil }
