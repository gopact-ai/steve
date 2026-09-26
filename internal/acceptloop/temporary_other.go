//go:build !linux

package acceptloop

import "syscall"

// platformTemporaries are the temporary Accept failures only Linux
// defines; there are none elsewhere.
var platformTemporaries []syscall.Errno
