//go:build unix

package checkpoint

import (
	"os"
	"syscall"
)

func lockStore(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
