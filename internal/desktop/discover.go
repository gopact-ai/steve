package desktop

import (
	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/config"
)

type DiscoveryOptions = agenttools.Options
type AgentCandidate = agenttools.Candidate

// DiscoverAgents only checks executable paths. It never opens agent settings,
// histories, credentials, sessions, or an agent process.
func DiscoverAgents(options DiscoveryOptions) []AgentCandidate { return agenttools.Discover(options) }

// ExecutablePath gives Finder launches explicit executable search roots,
// without running a login shell or its startup hooks.
func ExecutablePath(home string) string { return agenttools.ExecutablePath(home) }

func executable(path string) bool { return agenttools.Executable(path) }

// Registration builds the declaration after a user chooses a fresh candidate.
func Registration(candidate AgentCandidate) (config.Agent, config.Harness, error) {
	declared, err := agenttools.Registration(candidate)
	if err != nil {
		return config.Agent{}, config.Harness{}, err
	}
	return config.Agent{Harness: declared.Harness, Aliases: []string{candidate.ID}}, config.Harness{Adapter: declared.Adapter, Command: declared.Command, Args: declared.Args, Permission: config.PermissionRead}, nil
}
