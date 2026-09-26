//go:build unix

package node

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// failingListener fails Accept once fail is closed, the way a listener
// does when the process has run out of descriptors.
type failingListener struct {
	net.Listener
	fail   <-chan struct{}
	once   sync.Once
	closed chan struct{}
}

func failAccepting(l net.Listener, fail <-chan struct{}) *failingListener {
	return &failingListener{Listener: l, fail: fail, closed: make(chan struct{})}
}

func (l *failingListener) Accept() (net.Conn, error) {
	select {
	case <-l.fail:
		return nil, fmt.Errorf("accept: %w", syscall.EMFILE)
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *failingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// A broker that can no longer accept has let go of its socket by the time
// Serve returns: a launcher that connects afterwards is refused at once
// instead of waiting on a socket nobody accepts on.
func TestBrokerClosesItsSocketWhenAcceptFails(t *testing.T) {
	fail := make(chan struct{})
	close(fail)
	testHookSocketListener = func(l net.Listener) net.Listener { return failAccepting(l, fail) }
	t.Cleanup(func() { testHookSocketListener = nil })
	socket := filepath.Join(t.TempDir(), "mcp.sock")
	err := NewBroker(BrokerConfig{Socket: socket}).Serve(t.Context())
	if !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("Serve returned %v, want the accept failure", err)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		conn.Close()
		t.Fatal("the socket still takes connections after Serve returned")
	}
}
