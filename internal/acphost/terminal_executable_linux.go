//go:build linux

package acphost

import "os"

func openTerminalExecutable() (*os.File, string, func(), error) {
	self, err := os.Open("/proc/self/exe")
	return self, "/proc/self/fd/6", func() {}, err
}
