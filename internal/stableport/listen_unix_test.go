//go:build unix

package stableport

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"testing"
)

// On Unix, Listen resolves port 0 from the range.
func TestListenResolvesZeroFromTheRange(t *testing.T) {
	listener, err := Listen(func(network, address string) (net.Listener, error) {
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if port := listener.Addr().(*net.TCPAddr).Port; port < First || port > Last {
		t.Fatalf("port 0 resolved to %d, outside %d-%d", port, First, Last)
	}
}

// A port another listener serves on an overlapping address is not picked,
// even where the platform lets both bind it (macOS does, for a wildcard
// and a specific address, and for an IPv4-only wildcard and a dual-stack
// one): the other listener would take connections meant for this one, or
// this one those meant for it.
func TestSkipsAPortServedOnAnOverlappingAddress(t *testing.T) {
	for _, tc := range []struct{ network, held, asked string }{
		{"tcp", "127.0.0.1", "0.0.0.0"},
		{"tcp", "0.0.0.0", "127.0.0.1"},
		{"tcp4", "0.0.0.0", "0.0.0.0"},
		{"tcp4", "0.0.0.0", ""},
		{"tcp4", "0.0.0.0", "::"},
	} {
		t.Run(fmt.Sprintf("%s %s then %q", tc.network, tc.held, tc.asked), func(t *testing.T) {
			held, port := holdPortInRange(t, tc.network, tc.held)
			defer held.Close()
			listener, err := picker{candidate: sequence(port, candidate()), probe: probe}.listen(net.Listen, "tcp", net.JoinHostPort(tc.asked, "0"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if got := listener.Addr().(*net.TCPAddr).Port; got == port {
				t.Fatalf("%q:0 bound %d, which %s %s already serves", tc.asked, got, tc.network, held.Addr())
			}
		})
	}
}

// holdPortInRange listens on host, over network, at a free port of the
// range.
func holdPortInRange(t *testing.T, network, host string) (net.Listener, int) {
	t.Helper()
	start := rand.IntN(size)
	for i := range size {
		port := portAt((start + i) % size)
		listener, err := net.Listen(network, net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			return listener, port
		}
	}
	t.Fatal("no free port in the range")
	return nil, 0
}
