package node

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// gatedListener accepts connections whose SetDeadline, the first step of a
// handshake, only calls gate. The real socket gets no deadline; none is
// needed, since cancelling Serve closes the socket.
type gatedListener struct {
	net.Listener
	gate func()
}

func (l gatedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return gatedConn{conn, l.gate}, nil
}

type gatedConn struct {
	net.Conn
	gate func()
}

func (c gatedConn) SetDeadline(time.Time) error {
	c.gate()
	return nil
}

// Serve's shutdown runs cancel, closeMCP, closePluginRuntimes and
// handlers.Wait in that order, and waits for backgroundWG last. The gate
// holds a handshake before listenMCP until closePluginRuntimes has set
// pluginClosing, so listenMCP runs after closeMCP and before handlers.Wait
// returns. A reverse listener bound there would never be closed, and the
// forwardMCP serving it would keep backgroundWG, and Serve, waiting.
func TestServeReturnsWhenAHandshakeReachesTheReverseListenerAfterShutdownClosedIt(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var s *Server
	entered := make(chan struct{})
	var once sync.Once
	var heldUntilClosing atomic.Bool
	gate := func() {
		once.Do(func() { close(entered) })
		// Bounded, so a shutdown that never sets pluginClosing fails the
		// test instead of hanging it.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			s.pluginMu.Lock()
			closing := s.pluginClosing
			s.pluginMu.Unlock()
			if closing {
				heldUntilClosing.Store(true)
				return
			}
		}
	}
	s = NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir(), Listener: gatedListener{raw, gate}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	conn, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-entered:
	case err := <-served:
		t.Fatalf("Serve returned before a handshake began: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no handshake began")
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		// Closing again ends a listener bound after the first close, so
		// Serve returns before the test does.
		s.closeMCP()
		<-served
		t.Fatal("Serve did not return within 10s of cancel")
	}
	if !heldUntilClosing.Load() {
		t.Fatal("shutdown did not set pluginClosing while the handshake was held")
	}
}
