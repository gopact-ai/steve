package config

import (
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/project"
)

// ValidatePlugins checks desired scopes without touching package files or
// node credentials. Node readiness remains a separate observed fact.
func (c *Config) ValidatePlugins() error {
	occupied := map[string]string{}
	ids := make([]string, 0, len(c.Plugins))
	for id := range c.Plugins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		item := c.Plugins[id]
		if len(item.Targets) == 0 {
			return fmt.Errorf("plugin %s needs a target machine", id)
		}
		for node := range item.Targets {
			deployment, err := item.Deployment(id, node)
			if err != nil {
				return fmt.Errorf("plugin %s: %w", id, err)
			}
			level := c.HubLevel()
			if node != "" {
				configured, ok := c.Nodes[node]
				if !ok {
					return fmt.Errorf("plugin %s names unknown node %s", id, node)
				}
				level = project.Level(configured.Level).OrDefault()
			}
			for _, pid := range deployment.Projects {
				if item.Enabled {
					key := item.PackageID + "\x00" + node + "\x00" + pid
					if other, exists := occupied[key]; exists {
						return fmt.Errorf("plugins %s and %s enable the same package on the same project and node", other, id)
					}
					occupied[key] = id
				}
				p, ok := c.Projects[pid]
				if !ok {
					return fmt.Errorf("plugin %s names unknown project %s", id, pid)
				}
				if !project.Level(p.Level).OrDefault().Admits(level) {
					return fmt.Errorf("plugin %s: node %s may not hold project %s content", id, node, pid)
				}
			}
		}
	}
	return nil
}

// ClonePluginInstallations gives a configuration candidate sole ownership of
// its nested scope, target and secret-reference maps.
func ClonePluginInstallations(values map[string]plugins.Installation) map[string]plugins.Installation {
	if values == nil {
		return nil
	}
	cloned := make(map[string]plugins.Installation, len(values))
	for id, item := range values {
		item.Projects = slices.Clone(item.Projects)
		targets := make(map[string]plugins.Configuration, len(item.Targets))
		for node, cfg := range item.Targets {
			targets[node] = cfg.Clone()
		}
		item.Targets = targets
		cloned[id] = item
	}
	return cloned
}
