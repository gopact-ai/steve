package cluster

import (
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

// A node keeps the ports it first bound: its Raft and peer addresses are
// in the membership, its console origin is pinned, and its worker address
// is in the worker configuration, so a restart must bind them again. A
// port the kernel picks for ":0" comes from its ephemeral range, which it
// keeps handing to other sockets while the node is down. Ports here sit
// below every ephemeral range (Linux hands out 32768 and up, macOS 49152
// and up), so the kernel never gives one away on its own. The SSH link
// window, sshconnect.FirstLoopbackPort to LastLoopbackPort, is left out: a
// link binds those on a machine's loopback, and a node there that took one
// would shrink the window or be blocked by it while the link is up.
const (
	stablePortFirst = 20000
	stablePortLast  = 32767
	// stablePortAttempts bounds how many ports in the range are tried.
	stablePortAttempts = 64
)

// listenAtStablePort binds address with listen. An address with a
// nonzero port is bound as given. For port 0 it binds a random port in
// the stable range on the same host, trying another only when the port
// is in use; any other error is returned as is. If every attempt finds
// its port in use, it binds the address unchanged and the kernel picks an
// ephemeral port, which a later restart may find taken.
func listenAtStablePort(listen func(network, address string) (net.Listener, error), network, address string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "0" {
		return listen(network, address)
	}
	for range stablePortAttempts {
		listener, err := listen(network, net.JoinHostPort(host, strconv.Itoa(stablePortCandidate())))
		if !errors.Is(err, syscall.EADDRINUSE) {
			return listener, err
		}
	}
	return listen(network, address)
}

// stablePortCandidate picks uniformly from the stable range without the
// SSH link window.
func stablePortCandidate() int {
	window := sshconnect.LastLoopbackPort - sshconnect.FirstLoopbackPort + 1
	port := stablePortFirst + rand.IntN(stablePortLast-stablePortFirst+1-window)
	if port >= sshconnect.FirstLoopbackPort {
		port += window
	}
	return port
}
