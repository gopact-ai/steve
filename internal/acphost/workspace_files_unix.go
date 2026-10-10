//go:build unix

package acphost

import (
	"os"
	"syscall"
)

const workspaceFilesSupported = true

func openWorkspaceText(root *os.Root, rel string) (*os.File, error) {
	// A regular-file check after a blocking FIFO open would be too late.
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func workspaceSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func checkWorkspaceTextWritable(root *os.Root, rel string) error {
	file, err := root.OpenFile(rel, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return statErr
	}
	if !info.Mode().IsRegular() || !workspaceSingleLink(info) {
		return syscall.EINVAL
	}
	return closeErr
}
