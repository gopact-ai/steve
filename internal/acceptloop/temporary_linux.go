package acceptloop

import "syscall"

// platformTemporaries are the temporary Accept failures only Linux
// defines.
var platformTemporaries = []syscall.Errno{syscall.ENONET}
