package clustertest

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The addresses an enrollment test plans with stay bound until the test
// ends or hands them to what serves them. A port released early can be
// taken by any other process before its owner binds it.
func TestEnrollmentPortsStayBoundUntilHandedOver(t *testing.T) {
	ports := HoldEnrollmentPorts(t)
	for _, address := range []string{ports.Peer, ports.Raft} {
		if taken, err := net.Listen("tcp", address); err == nil {
			taken.Close()
			t.Fatalf("%s was free for anyone to bind before its owner took it", address)
		}
	}
}

// Listen hands a held socket to whoever asks for its port, whatever host
// the request names, and only once; other addresses are bound afresh.
func TestListenHandsOverAHeldPortOnce(t *testing.T) {
	ports := HoldEnrollmentPorts(t)
	_, port, _ := net.SplitHostPort(ports.Raft)
	first, err := ports.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	if err != nil {
		t.Fatalf("the held port was not handed over: %v", err)
	}
	defer first.Close()
	if first.Addr().String() != ports.Raft {
		t.Fatalf("handed %s for the held %s", first.Addr(), ports.Raft)
	}
	if second, err := ports.Listen("tcp", ports.Raft); err == nil {
		second.Close()
		t.Fatal("a port already handed over was handed over again")
	}
	other, err := ports.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("an address that is not held was not bound: %v", err)
	}
	other.Close()
}

// A child process serves the held sockets it inherits as files, at the
// same addresses.
func TestInheritedPortsServeTheParentsSockets(t *testing.T) {
	ports := HoldEnrollmentPorts(t)
	files := ports.Files(t)
	inherited, err := Inherit(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{ports.Raft, ports.Peer} {
		listener, err := inherited.Listen("tcp", address)
		if err != nil {
			t.Fatalf("inherited %s was not handed over: %v", address, err)
		}
		if listener.Addr().String() != address {
			t.Fatalf("inherited %s served at %s", address, listener.Addr())
		}
		listener.Close()
	}
}

// Inherit closes every file it was given, including those after one that
// is not a listening socket.
func TestInheritClosesEveryFileWhenOneIsNotASocket(t *testing.T) {
	ports := HoldEnrollmentPorts(t)
	held := ports.Files(t)
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-socket"))
	if err != nil {
		t.Fatal(err)
	}
	files := append([]*os.File{plain}, held...)
	if inherited, err := Inherit(files); err == nil {
		inherited.Release()
		t.Fatal("a file that is not a socket was inherited")
	}
	for _, file := range files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			file.Close()
			t.Errorf("%s was left open: %v", file.Name(), err)
		}
	}
}
