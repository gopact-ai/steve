//go:build !linux

package fleetlab

import (
	"errors"
	"os/exec"
)

type ownedProcess struct {
	done chan struct{}
	err  error
}

func startOwned(*exec.Cmd, string) (*ownedProcess, error) {
	return nil, errors.New("hub lab requires Linux")
}
func (*ownedProcess) stop() error { return nil }
