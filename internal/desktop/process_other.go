//go:build !unix

package desktop

import (
	"errors"
	"os"
	"os/exec"
)

func detach(*exec.Cmd)      {}
func processAlive(int) bool { return false }
func openPrivateLog(string) (*os.File, error) {
	return nil, errors.New("desktop startup is unsupported on this operating system")
}
