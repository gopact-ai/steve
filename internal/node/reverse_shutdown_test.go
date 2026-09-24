package node

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// Once closeMCP has run, listenMCP returns errMCPClosed and no listener,
// whether or not one was bound before. Shutdown waits for the goroutine
// serving the listener it closed, and nothing would close one bound
// afterwards.
func TestReverseMCPListenerIsNotBoundOnceShutdownClosedIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		boundBefore bool
	}{{"never bound", false}, {"bound before", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir()})
			// Closing again ends whatever a failing run bound.
			t.Cleanup(s.closeMCP)
			if tc.boundBefore {
				if _, err := s.listenMCP(); err != nil {
					t.Fatal(err)
				}
			}
			s.closeMCP()
			listener, err := s.listenMCP()
			if listener != nil {
				t.Fatalf("listenMCP after closeMCP returned %s", listener.Addr())
			}
			if !errors.Is(err, errMCPClosed) {
				t.Fatalf("listenMCP after closeMCP: %v, want %v", err, errMCPClosed)
			}
		})
	}
}

// Once shutdown has closed the reverse listener, a handshake claims nothing
// and sends no advert.
func TestAHandshakeAfterShutdownClosedTheReverseListenerClaimsNothing(t *testing.T) {
	s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir()})
	t.Cleanup(s.closeMCP)
	s.closeMCP()
	node, hub := net.Pipe()
	t.Cleanup(func() { _ = hub.Close() })
	dialed := make(chan error, 1)
	go func() {
		_, err := nodewire.Dial(hub, nodewire.Hello{Hub: "hub", Token: "token"})
		dialed <- err
	}()
	claim := &hubClaim{}
	_, ok := s.handshake(node, claim)
	// As handle does after a failed handshake, close the node's end; a Dial
	// still waiting on the pipe then returns.
	_ = node.Close()
	var dialErr error
	select {
	case dialErr = <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("Dial did not return after the node closed its end")
	}
	if ok || claim.claimed {
		t.Fatalf("handshake after closeMCP: ok=%v claimed=%v", ok, claim.claimed)
	}
	if dialErr == nil {
		t.Fatal("Dial received an advert")
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
