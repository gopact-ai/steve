package sshconnect

import (
	"io"
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
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: l.Addr().Network(), Addr: l.Addr(), Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

// A forward whose process is out of descriptors for a moment keeps
// accepting once they free up. With no session attached, an accepted
// connection is closed at once, so the dialer reads the end of it rather
// than waiting on a listener nobody accepts on.
func TestBridgeKeepsAcceptingThroughATemporaryAcceptFailure(t *testing.T) {
	b := newBridge(nil)
	b.bind = func(network, address string) (net.Listener, error) {
		l, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		f := &flakyListener{Listener: l}
		f.failures.Store(3)
		return f, nil
	}
	defer b.close()
	address, err := b.listen(0, PortForward{Target: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read %v from the forward, want it closed at once: the connection was never accepted", err)
	}
}
