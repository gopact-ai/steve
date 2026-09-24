package config

import (
	"maps"
	"slices"

	"github.com/gopact-ai/steve/internal/plugins"
)

// Clone is a copy of c that shares no map, slice or pointer with it, so
// the copy can be edited while c is read. Empty and absent collections
// stay as they were: the copy saves to the same file.
func (c *Config) Clone() *Config {
	next := *c
	next.Plugins = cloneValues(c.Plugins, cloneInstallation)
	next.RuntimePermissions = maps.Clone(c.RuntimePermissions)
	if c.RuntimeHome != nil {
		home := *c.RuntimeHome
		next.RuntimeHome = &home
	}
	next.Agents = cloneValues(c.Agents, Agent.clone)
	next.Projects = cloneValues(c.Projects, Project.clone)
	next.Harnesses = cloneValues(c.Harnesses, Harness.clone)
	next.Nodes = maps.Clone(c.Nodes)
	next.MCPServers = cloneValues(c.MCPServers, MCPServer.clone)
	next.Feishu = c.Feishu.clone()
	next.Gateway = c.Gateway.clone()
	return &next
}

func cloneValues[K comparable, V any](in map[K]V, clone func(V) V) map[K]V {
	if in == nil {
		return nil
	}
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = clone(v)
	}
	return out
}

func cloneInstallation(in plugins.Installation) plugins.Installation {
	in.Projects = slices.Clone(in.Projects)
	in.Targets = cloneValues(in.Targets, plugins.Configuration.Clone)
	return in
}

func (a Agent) clone() Agent {
	a.PluginOrigin = a.PluginOrigin.Clone()
	a.Aliases = slices.Clone(a.Aliases)
	a.Options = maps.Clone(a.Options)
	a.Requires = slices.Clone(a.Requires)
	a.Skills = slices.Clone(a.Skills)
	a.MCPServers = slices.Clone(a.MCPServers)
	return a
}

func (p Project) clone() Project {
	p.Skills = slices.Clone(p.Skills)
	p.DurablePlaces = slices.Clone(p.DurablePlaces)
	p.Grants = maps.Clone(p.Grants)
	p.Workspaces = slices.Clone(p.Workspaces)
	return p
}

func (h Harness) clone() Harness {
	h.Args = slices.Clone(h.Args)
	h.Env = slices.Clone(h.Env)
	return h
}

func (m MCPServer) clone() MCPServer {
	m.Args = slices.Clone(m.Args)
	m.Env = maps.Clone(m.Env)
	m.Headers = maps.Clone(m.Headers)
	return m
}

func (f Feishu) clone() Feishu {
	if f.Enabled != nil {
		enabled := *f.Enabled
		f.Enabled = &enabled
	}
	f.AllowedSenders = slices.Clone(f.AllowedSenders)
	f.BlockedSenders = slices.Clone(f.BlockedSenders)
	return f
}

func (g Gateway) clone() Gateway {
	g.Peers = maps.Clone(g.Peers)
	g.Capabilities = slices.Clone(g.Capabilities)
	g.Tools = slices.Clone(g.Tools)
	g.Declares = slices.Clone(g.Declares)
	g.Regions = maps.Clone(g.Regions)
	return g
}
