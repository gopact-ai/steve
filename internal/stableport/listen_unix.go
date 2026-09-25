//go:build unix

package stableport

import "syscall"

// picksPorts is whether Listen resolves port 0 from the range.
const picksPorts = true

// clearReuseAddr turns SO_REUSEADDR off on a socket before it binds.
func clearReuseAddr(network, address string, c syscall.RawConn) error {
	var err error
	if controlErr := c.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
	}); controlErr != nil {
		return controlErr
	}
	return err
}
