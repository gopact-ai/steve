//go:build !linux && !darwin

package acphost

import (
	"context"
	"os"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/procgroup"
)

type terminalChildConfig struct {
	Command string
	Args    []string
	Env     []string
	Mark    string
}

func localTerminalAvailable(Process) bool { return false }
func validateLocalTerminal(Process) error { return procgroup.ErrUnsupported }
func localTerminalConfig(Process, *acp.CreateTerminalRequest) (terminalChildConfig, error) {
	return terminalChildConfig{}, procgroup.ErrUnsupported
}
func prepareLocalTerminal(context.Context, Process, *os.File, terminalChildConfig, *terminalOutput, []string) (terminalRuntime, error) {
	return nil, procgroup.ErrUnsupported
}

func openTerminalDirectory(root *os.Root, rel string) (*os.File, error) {
	return nil, procgroup.ErrUnsupported
}
