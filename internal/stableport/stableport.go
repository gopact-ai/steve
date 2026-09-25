// Package stableport binds listeners whose port outlives the process. A
// port picked for ":0" is written down after the first bind (a membership
// address, a pinned console origin, a remembered MCP port) and bound again
// on every start, so it has to be one the kernel does not hand to other
// sockets in the meantime.
package stableport

import (
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
// is returned as is. If every attempt finds its port in use, it binds the
// address unchanged and the kernel picks an ephemeral port, which a later
// restart may find taken.
func Listen(listen func(network, address string) (net.Listener, error), network, address string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "0" {
		return listen(network, address)
	}
	for range attempts {
		listener, err := listen(network, net.JoinHostPort(host, strconv.Itoa(candidate())))
		if !errors.Is(err, syscall.EADDRINUSE) {
			return listener, err
		}
	}
	return listen(network, address)
}

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
