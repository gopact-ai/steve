package cluster

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

// The lowest port any supported platform's default ephemeral range starts
// at (Linux 32768; macOS 49152).
const lowestDefaultEphemeralPort = 32768

type addressListener struct{ address string }

func (l addressListener) Accept() (net.Conn, error) { return nil, errors.New("not serving") }
func (l addressListener) Close() error              { return nil }
func (l addressListener) Addr() net.Addr {
	host, port, _ := net.SplitHostPort(l.address)
	n, _ := strconv.Atoi(port)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: n}
}

func addressInUse(address string) error {
	return &net.OpError{Op: "listen", Net: "tcp", Err: fmt.Errorf("bind %s: %w", address, syscall.EADDRINUSE)}
}

func TestStablePortKeepsAnExplicitPort(t *testing.T) {
	refused := errors.New("refused")
	var asked []string
	_, err := listenAtStablePort(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return nil, refused
	}, "tcp", "127.0.0.1:7801")
	if !errors.Is(err, refused) || len(asked) != 1 || asked[0] != "127.0.0.1:7801" {
		t.Fatalf("explicit port: asked %v, err %v", asked, err)
	}
}

func TestStablePortResolvesZeroOutsideEphemeralRangesAndLinkPorts(t *testing.T) {
	seen := map[int]bool{}
	for range 2000 {
		listener, err := listenAtStablePort(func(network, address string) (net.Listener, error) {
			return addressListener{address}, nil
		}, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().(*net.TCPAddr)
		if !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
			t.Fatalf("host changed: %s", address)
		}
		port := address.Port
		if port < 1024 || port >= lowestDefaultEphemeralPort {
			t.Fatalf("port %d is not below every default ephemeral range", port)
		}
		if port >= sshconnect.FirstLoopbackPort && port <= sshconnect.LastLoopbackPort {
			t.Fatalf("port %d is one an SSH link binds on loopback", port)
		}
		seen[port] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct ports in 2000 draws", len(seen))
	}
}

func TestStablePortRetriesOnlyAPortInUse(t *testing.T) {
	var asked []string
	listener, err := listenAtStablePort(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if len(asked) <= 3 {
			return nil, addressInUse(address)
		}
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0")
	if err != nil || len(asked) != 4 || listener.Addr().String() != asked[3] {
		t.Fatalf("in-use ports: asked %v, err %v", asked, err)
	}

	refused := errors.New("permission denied")
	asked = nil
	if _, err := listenAtStablePort(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return nil, refused
	}, "tcp", "127.0.0.1:0"); !errors.Is(err, refused) || len(asked) != 1 {
		t.Fatalf("other bind errors must not be retried: asked %v, err %v", asked, err)
	}
}

// When every attempt in the range finds its port in use, the kernel picks
// as it did before: the node still starts, at an ephemeral port.
func TestStablePortFallsBackToTheKernelWhenAttemptsRunOut(t *testing.T) {
	var asked []string
	listener, err := listenAtStablePort(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if address == "127.0.0.1:0" {
			return addressListener{"127.0.0.1:54321"}, nil
		}
		return nil, addressInUse(address)
	}, "tcp", "127.0.0.1:0")
	if err != nil || listener.Addr().String() != "127.0.0.1:54321" {
		t.Fatalf("fallback: %v %v", listener, err)
	}
	if len(asked) != stablePortAttempts+1 || asked[len(asked)-1] != "127.0.0.1:0" {
		t.Fatalf("asked %d addresses, last %q; want %d then the kernel", len(asked), asked[len(asked)-1], stablePortAttempts)
	}
}
