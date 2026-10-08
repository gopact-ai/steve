//go:build !linux && !darwin

package acphost

import "errors"

const TerminalChildVerb = "--acp-terminal-child"

func RunTerminalChild(args []string) (bool, error) {
	if len(args) > 0 && args[0] == TerminalChildVerb {
		return true, errors.New("owned terminal execution is unavailable on this platform")
	}
	return false, nil
}

func (p *localProcess) closeTerminalAdmission() {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
}
