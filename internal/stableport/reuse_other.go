//go:build !unix

package stableport

import "syscall"

// Other platforms bind without SO_REUSEADDR already.
var clearReuseAddr func(network, address string, c syscall.RawConn) error
