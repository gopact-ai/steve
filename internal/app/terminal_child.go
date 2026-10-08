package app

import "github.com/gopact-ai/steve/internal/acphost"

// RunTerminalChild handles the inherited-descriptor helper before application
// configuration or provider initialization. Ordinary invocations return false.
func RunTerminalChild(args []string) (bool, error) {
	return acphost.RunTerminalChild(args)
}
