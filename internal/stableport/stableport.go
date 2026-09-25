// Package stableport binds listeners whose port outlives the process. A
// port picked for ":0" is written down after the first bind (a membership
// address, a pinned console origin, a remembered MCP port) and bound again
// on every start, so it has to be one the kernel does not hand to other
// sockets in the meantime.
package stableport

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"
)

// A port the kernel picks for ":0" comes from its ephemeral range, which
// it keeps handing to other sockets while the process is down. Ports from
// First to Last sit below every ephemeral range (Linux hands out 32768 and
// up, macOS 49152 and up), so the kernel never gives one away on its own.
// The SSH link window, LinkFirst to LinkLast, is left out: a link binds
// those on a machine's loopback, and a node there that took one would
// shrink the window or be blocked by it while the link is up.
const (
	First = 20000
	Last  = 32767

	LinkFirst = 25407
	LinkLast  = 25426

	// attempts bounds how many ports in the range are tried.
	attempts = 64
)

// Listen binds address with listen. An address with a nonzero port is
// bound as given. For port 0 it binds a random port in the range on the
// same host, trying another only when the port is in use; any other error
// is returned as is. A port another socket listens on at an overlapping
// address counts as in use, even where the platform would let both bind
// it. If every attempt finds its port in use, it binds the address
// unchanged and the kernel picks an ephemeral port, which a later restart
// may find taken.
func Listen(listen func(network, address string) (net.Listener, error), network, address string) (net.Listener, error) {
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
	if err != nil || port != "0" {
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
	return listen(network, address)
}

// probe binds address without SO_REUSEADDR and closes it again.
// Listeners set SO_REUSEADDR, and on macOS that lets a wildcard and a
// specific address share a port: 0.0.0.0:P binds while another socket
// listens on 127.0.0.1:P, which then takes the loopback connections, and
// the other way round. Without SO_REUSEADDR either bind is refused as in
// use. The listener that is kept is bound by the caller as usual, so a
// restart can bind its port again past connections in TIME_WAIT; the
// probe refuses those too, which only skips a port.
func probe(network, address string) error {
	listener, err := exclusive.Listen(context.Background(), network, address)
	if err != nil {
		return err
	}
	return listener.Close()
}

var exclusive = net.ListenConfig{Control: clearReuseAddr}

// size is how many ports the range holds without the SSH link window.
const size = Last - First + 1 - (LinkLast - LinkFirst + 1)

// candidate picks uniformly from the range without the SSH link window.
func candidate() int {
	return portAt(rand.IntN(size))
}

// portAt is the i-th port of the range, counting from First and skipping
// the SSH link window, for i from 0 to size-1.
func portAt(i int) int {
	port := First + i
	if port >= LinkFirst {
		port += LinkLast - LinkFirst + 1
	}
	return port
}
