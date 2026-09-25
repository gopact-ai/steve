package agentmcp

import (
	"testing"

	"github.com/gopact-ai/steve/internal/stableport"
)

func requireStablePort(t *testing.T, what string, port int) {
	t.Helper()
	if port < stableport.First || port > stableport.Last || port >= stableport.LinkFirst && port <= stableport.LinkLast {
		t.Fatalf("%s bound port %d, outside the stable range", what, port)
	}
}

// The messaging server's port is remembered and handed back on restart, so
// a port it picks, first or because the remembered one is taken, comes
// from the stable range.
func TestNewPicksItsPortFromTheStableRange(t *testing.T) {
	first, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.listener.Close()
	requireStablePort(t, "first start", first.Port())
	moved, err := New(first.Port())
	if err != nil {
		t.Fatal(err)
	}
	defer moved.listener.Close()
	if moved.Port() == first.Port() {
		t.Fatalf("two servers on port %d", moved.Port())
	}
	requireStablePort(t, "start with the remembered port taken", moved.Port())
}
