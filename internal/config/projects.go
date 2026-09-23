package config

import (
	"maps"
	"slices"
)

// CloneProjects isolates candidate declaration maps/slices from the live config.
func CloneProjects(c *Config) *Config {
	next := *c
	next.Projects = maps.Clone(c.Projects)
	if next.Projects == nil {
		next.Projects = map[string]Project{}
	}
	for id, p := range next.Projects {
		p.Skills, p.DurablePlaces = slices.Clone(p.Skills), slices.Clone(p.DurablePlaces)
		p.Workspaces, p.Grants = slices.Clone(p.Workspaces), maps.Clone(p.Grants)
		next.Projects[id] = p
	}
	return &next
}
