package desktop

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/config"
)

// EnrollDiscovered registers the discovered, runnable agents of a fresh
// installation. A configuration that already lists agents belongs to its owner
// and is left untouched; Resources manages later additions. Agents start with
// read-only permission and the first one becomes the default.
func EnrollDiscovered(configPath string, candidates []AgentCandidate) ([]string, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}
	if len(cfg.Agents) > 0 {
		return nil, nil
	}
	agents := map[string]config.Agent{}
	harnesses := map[string]config.Harness{}
	for name, h := range cfg.Harnesses {
		harnesses[name] = h
	}
	var added []string
	for _, candidate := range candidates {
		if !candidate.Installed || len(candidate.Requires) > 0 {
			continue
		}
		if _, taken := harnesses[candidate.Harness]; taken {
			continue
		}
		registered, tool, err := Registration(candidate)
		if err != nil {
			continue
		}
		registered.Default = len(added) == 0
		agents[candidate.ID] = registered
		harnesses[candidate.Harness] = tool
		added = append(added, candidate.Name)
	}
	if len(added) == 0 {
		return nil, nil
	}
	cfg.Agents, cfg.Harnesses = agents, harnesses
	if err := config.Save(configPath, cfg); err != nil {
		return nil, fmt.Errorf("save configuration: %w", err)
	}
	return added, nil
}
