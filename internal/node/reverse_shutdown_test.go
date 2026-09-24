package node

import (
	"net"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// Shutdown closes the reverse MCP listener and later waits for the
// goroutine serving it. A handshake still in flight must not bind another
// listener after that close: nothing would close it, and Serve would wait
// for it forever.
func TestReverseMCPListenerIsNotBoundOnceShutdownClosedIt(t *testing.T) {
	for name, boundBefore := range map[string]bool{"never bound": false, "bound before": true} {
		t.Run(name, func(t *testing.T) {
			s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir()})
			// Closing again ends whatever a failing run bound.
			t.Cleanup(s.closeMCP)
			if boundBefore {
				if _, err := s.listenMCP(); err != nil {
					t.Fatal(err)
				}
			}
			s.closeMCP()
			if listener, err := s.listenMCP(); err == nil {
				t.Fatalf("listenMCP after closeMCP returned %s", listener.Addr())
			}
		})
	}
}

// A node that has begun shutting down takes no hub: the handshake stops
// before the advert and claims nothing.
func TestAHandshakeAfterShutdownClosedTheReverseListenerClaimsNothing(t *testing.T) {
	s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir()})
	t.Cleanup(s.closeMCP)
	s.closeMCP()
	node, hub := net.Pipe()
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(func() { _ = node.Close() })
	go func() {
		_, _ = nodewire.Dial(hub, nodewire.Hello{Hub: "hub", Token: "token"})
	}()
	claim := &hubClaim{}
	if _, ok := s.handshake(node, claim); ok || claim.claimed {
		t.Fatalf("handshake after closeMCP: ok=%v claimed=%v", ok, claim.claimed)
	}
}
