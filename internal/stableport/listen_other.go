//go:build !unix

package stableport

import "syscall"

// picksPorts is false here, so Listen hands port 0 to the kernel: a bind
// error for a port in use or reserved does not match syscall.EADDRINUSE on
// these platforms, so a random pick could fail where the kernel's pick
// does not.
const picksPorts = false

// Other platforms bind without SO_REUSEADDR already.
var clearReuseAddr func(network, address string, c syscall.RawConn) error
