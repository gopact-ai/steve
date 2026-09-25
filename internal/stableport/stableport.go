// Package stableport binds listeners whose port outlives the process. A
// port picked for ":0" is written down after the first bind (a membership
// address, a pinned console origin, a remembered MCP port) and bound again
// on every start, so it has to be one the kernel does not hand to other
// sockets in the meantime. On Unix, Listen picks such a port from a fixed
// range; on other platforms the kernel still picks it.
package stableport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"
)

// A port the kernel picks for ":0" comes from its ephemeral range, which
// it keeps handing to other sockets while the process is down. Ports from
// first to last sit below the default ephemeral ranges (Linux hands out
// 32768 and up, macOS 49152 and up), so with those defaults the kernel
// never gives one away on its own; a Linux ip_local_port_range lowered
// into the range undoes that.
// The range also ends below the Kubernetes NodePort default range
// (30000-32767): on a Kubernetes node kube-proxy forwards those ports, a
// bind there need not find one in use, and connections to it would be
// forwarded away. The SSH link window, LinkFirst to LinkLast, is left
// out: a link binds those on a machine's loopback, and a node there that
// took one would shrink the window or be blocked by it while the link is
// up.
const (
	first = 20000
	last  = 29999

	LinkFirst = 25407
	LinkLast  = 25426

	// attempts bounds how many ports in the range are tried.
	attempts = 64
)

// InRange reports whether port is one Listen may resolve port 0 to on
// Unix: a port of the range outside the SSH link window. When Listen falls
// back to the kernel, the port it returns need not be in the range.
func InRange(port int) bool {
	return port >= first && port <= last && (port < LinkFirst || port > LinkLast)
}

// Listen binds address with listen. An address with a nonzero port is
// bound as given. For port 0, or an empty port, it binds a random port in
// the range on the same host, trying another only when the port is in
// use; any other error is returned as is. A port another socket listens
// on at an overlapping address counts as in use, even where the platform
// would let both bind it. If every attempt finds its port in use, it
// binds the address unchanged and the kernel picks an ephemeral port,
// which a later restart may find taken. On platforms other than Unix it
// binds every address unchanged, port 0 included; see picksPorts.
func Listen(listen func(network, address string) (net.Listener, error), network, address string) (net.Listener, error) {
	if !picksPorts {
		return listen(network, address)
	}
	return picker{candidate: candidate, probe: probe}.listen(listen, network, address)
}

// picker resolves port 0 from candidate, skipping a candidate probe finds
// in use before listen binds it.
type picker struct {
	candidate func() int
	probe     func(network, address string) error
}

func (p picker) listen(listen func(network, address string) (net.Listener, error), network, address string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	// An empty port is port 0 to net.Listen as well.
	if err != nil || port != "0" && port != "" {
		return listen(network, address)
	}
	for range attempts {
		candidate := net.JoinHostPort(host, strconv.Itoa(p.candidate()))
		if errors.Is(p.probe(network, candidate), syscall.EADDRINUSE) {
			continue
		}
		listener, err := listen(network, candidate)
		if !errors.Is(err, syscall.EADDRINUSE) {
			return listener, err
		}
	}
	slog.Warn(fmt.Sprintf("stableport: %d ports in %d-%d were in use; %s takes a port the system picks, which a restart may find taken", attempts, first, last, address), "network", network)
	return listen(network, address)
}

// probe binds address without SO_REUSEADDR and closes it again.
// Listeners set SO_REUSEADDR, and on macOS that lets a wildcard and a
// specific address share a port: 0.0.0.0:P binds while another socket
// listens on 127.0.0.1:P, which then takes the loopback connections, and
// the other way round. Without SO_REUSEADDR either bind is refused as in
// use. A "tcp" wildcard binds one dual-stack socket, which macOS lets
// share a port with an IPv4-only wildcard listener even without
// SO_REUSEADDR, and that listener then takes the IPv4 connections; so for
// a wildcard the probe also binds the IPv4 wildcard.
// Probing and binding are two steps: a socket that binds the port in
// between can still share it on macOS. The listener that is kept is bound
// by the caller as usual, so a restart can bind its port again past
// connections in TIME_WAIT; the probe refuses those too, which only skips
// a port.
func probe(network, address string) error {
	if err := bindAndClose(network, address); err != nil || network != "tcp" {
		return err
	}
	host, port, _ := net.SplitHostPort(address)
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		return bindAndClose("tcp4", net.JoinHostPort("0.0.0.0", port))
	}
	return nil
}

func bindAndClose(network, address string) error {
	listener, err := exclusive.Listen(context.Background(), network, address)
	if err != nil {
		return err
	}
	return listener.Close()
}

var exclusive = net.ListenConfig{Control: clearReuseAddr}

// size is how many ports the range holds without the SSH link window.
const size = last - first + 1 - (LinkLast - LinkFirst + 1)

// candidate picks uniformly from the range without the SSH link window.
func candidate() int {
	return portAt(rand.IntN(size))
}

// portAt is the i-th port of the range, counting from first and skipping
// the SSH link window, for i from 0 to size-1.
func portAt(i int) int {
	port := first + i
	if port >= LinkFirst {
		port += LinkLast - LinkFirst + 1
	}
	return port
}
