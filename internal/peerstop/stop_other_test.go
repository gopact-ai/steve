//go:build !linux

package peerstop

import (
	"errors"
	"testing"
)

// No path or claimed identity can enable numeric-PID stopping on platforms
// without the stable installation stop protocol.
func TestStopRequiresStablePlatformHandles(t *testing.T) {
	stopped, err := Stop(t.Context(), "untrusted-sidecar", "cluster", "node")
	if stopped || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Stop = %v, %v; want false, ErrUnsupported", stopped, err)
	}
}
