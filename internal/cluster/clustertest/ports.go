// Package clustertest holds the ports a test enrolls a machine at, so no
// other process can take them between planning the enrollment and the
// machine's node serving them.
package clustertest

import (
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
)

// Ports are listeners bound in advance. Listen hands each one over to the
// first request for its port; PeerOptions.Listen takes it as is.
type Ports struct {
	// Peer and Raft are the addresses of the held peer and Raft sockets.
	Peer, Raft string
	mu         sync.Mutex
	held       map[string]net.Listener
}

// HoldEnrollmentPorts binds a machine's peer and Raft ports on loopback.
// Those not handed over are closed when the test ends.
func HoldEnrollmentPorts(t testing.TB) *Ports {
	t.Helper()
	ports := &Ports{held: map[string]net.Listener{}}
	for _, address := range []*string{&ports.Peer, &ports.Raft} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*address = listener.Addr().String()
		ports.held[port(*address)] = listener
	}
	t.Cleanup(ports.Release)
	return ports
}

// Inherit serves the Raft and peer listeners a parent process passed as
// files, in the order Files gives them. It closes every file, whether or
// not it succeeds.
func Inherit(files []*os.File) (*Ports, error) {
	ports := &Ports{held: map[string]net.Listener{}}
	addresses := []*string{&ports.Raft, &ports.Peer}
	for i, file := range files {
		listener, err := net.FileListener(file)
		file.Close()
		if err != nil {
			for _, rest := range files[i+1:] {
				rest.Close()
			}
			ports.Release()
			return nil, err
		}
		if i < len(addresses) {
			*addresses[i] = listener.Addr().String()
		}
		ports.held[port(listener.Addr().String())] = listener
	}
	return ports, nil
}

// Listen hands over the held listener at address's port, whatever host
// address names; the listener is the caller's from then on. A port
// already handed over is in use. Other addresses are bound on the network.
func (p *Ports) Listen(network, address string) (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := port(address)
	listener, ok := p.held[key]
	if !ok {
		return net.Listen(network, address)
	}
	if listener == nil {
		return nil, &net.OpError{Op: "listen", Net: network, Err: fmt.Errorf("bind %s: %w", address, syscall.EADDRINUSE)}
	}
	p.held[key] = nil
	return listener, nil
}

// Files are copies of the held Raft and peer sockets, in that order, for
// a child process to inherit. The caller closes them once the child has
// started.
func (p *Ports) Files(t testing.TB) []*os.File {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var files []*os.File
	for _, address := range []string{p.Raft, p.Peer} {
		listener, ok := p.held[port(address)].(*net.TCPListener)
		if !ok {
			t.Fatalf("%s is not held", address)
		}
		file, err := listener.File()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// Release closes the listeners not handed over.
func (p *Ports) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, listener := range p.held {
		if listener != nil {
			listener.Close()
			p.held[key] = nil
		}
	}
}

func port(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "0" {
		return address
	}
	return port
}
