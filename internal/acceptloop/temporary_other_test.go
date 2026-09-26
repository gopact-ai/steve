//go:build !linux

package acceptloop

import "syscall"

// linuxOnlyTemporaries are the temporary Accept failures only Linux
// defines; there are none elsewhere.
var linuxOnlyTemporaries []syscall.Errno
