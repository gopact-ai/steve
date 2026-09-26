package node

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// flakyListener fails its first accepts with EMFILE, the way a listener
// does while the process is out of descriptors, and then accepts as the
// listener it wraps does.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
	accepted chan struct{}
}

func flaky(l net.Listener, failures int32) *flakyListener {
	f := &flakyListener{Listener: l, accepted: make(chan struct{}, 1)}
	f.failures.Store(failures)
	return f
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: l.Addr().Network(), Addr: l.Addr(), Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	conn, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.accepted <- struct{}{}:
		default:
		}
	}
	return conn, err
}

// waitAccepted waits until the listener has handed out a connection,
// failing if served reports the loop's end first.
func (l *flakyListener) waitAccepted(t *testing.T, served <-chan error) {
	t.Helper()
	select {
	case <-l.accepted:
	case err := <-served:
		t.Fatalf("serving ended on a temporary accept failure: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no connection was accepted after the temporary failures")
	}
}

// A node out of descriptors for a moment keeps its listener: accepting
// resumes once descriptors free up, instead of Serve returning and the
// node leaving the hub's reach until it is restarted.
func TestServeKeepsAcceptingThroughATemporaryAcceptFailure(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := flaky(raw, 3)
	s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir(), Listener: listener})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	conn, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	listener.waitAccepted(t, served)
	conn.Close()
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
