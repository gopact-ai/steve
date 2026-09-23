package coordination

import (
	"errors"
	"strings"
	"testing"
)

// A malformed Raft address is reported as malformed, not as a request for
// an authenticated transport.
func TestRequireLoopbackNamesAMalformedAddress(t *testing.T) {
	err := requireLoopback("127.0.0.1")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "invalid Raft address") {
		t.Fatalf("requireLoopback(no port) = %v", err)
	}
	err = requireLoopback("10.0.0.1:7000")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "StreamLayer") {
		t.Fatalf("requireLoopback(remote) = %v", err)
	}
	if err := requireLoopback("127.0.0.1:7000"); err != nil {
		t.Fatalf("requireLoopback(loopback) = %v", err)
	}
}
