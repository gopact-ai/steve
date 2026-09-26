//go:build linux

package acceptloop

import "syscall"

// linuxOnlyTemporaries are the temporary Accept failures only Linux
// defines.
var linuxOnlyTemporaries = []syscall.Errno{syscall.ENONET}
