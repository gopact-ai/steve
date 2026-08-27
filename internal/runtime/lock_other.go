//go:build !unix

package runtime

// AcquireLock is a no-op where flock is unavailable; the singleton guarantee
// is unix-only for now.
func AcquireLock(string) (func(), error) { return func() {}, nil }
