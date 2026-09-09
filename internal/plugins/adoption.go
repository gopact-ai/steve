package plugins

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
)

// Adoption replaces this Agent's legacy attachments, leaving the reusable
// source files and machine MCP definitions under their existing owner.
type Adoption struct {
	Skills map[string]string `json:"skills,omitempty"`
	MCP    map[string]string `json:"mcp,omitempty"`
}

func (a *Adoption) Clone() *Adoption {
	if a == nil {
		return nil
	}
	return &Adoption{Skills: maps.Clone(a.Skills), MCP: maps.Clone(a.MCP)}
}

func (a Adoption) Check(template AgentPreset, skills, servers []string) error {
	seen := map[string]bool{}
	for path, capability := range a.Skills {
		name := filepath.Base(path)
		if !filepath.IsAbs(path) || !nameShape.MatchString(name) || !slices.Contains(skills, path) || !slices.Contains(template.Skills, capability) || seen[name] {
			return fmt.Errorf("%w: skill adoption must name an existing Agent attachment and a preset skill", ErrInvalid)
		}
		seen[name] = true
	}
	for name, capability := range a.MCP {
		if !slices.Contains(servers, name) || !slices.Contains(template.MCPServers, capability) {
			return fmt.Errorf("%w: MCP adoption must name an existing Agent attachment and a preset server", ErrInvalid)
		}
	}
	return nil
}

func (a *Adoption) Remaining(skills, servers []string) ([]string, []string) {
	if a == nil {
		return slices.Clone(skills), slices.Clone(servers)
	}
	return slices.DeleteFunc(slices.Clone(skills), func(path string) bool { _, ok := a.Skills[path]; return ok }),
		slices.DeleteFunc(slices.Clone(servers), func(name string) bool { _, ok := a.MCP[name]; return ok })
}

func (a *Adoption) ExcludedSkills() []string {
	if a == nil {
		return nil
	}
	names := make([]string, 0, len(a.Skills))
	for path := range a.Skills {
		names = append(names, filepath.Base(path))
	}
	slices.Sort(names)
	return names
}

func (a *Adoption) CheckRemaining(skills, servers []string) error {
	if a == nil {
		return nil
	}
	for _, path := range skills {
		if _, ok := a.Skills[path]; ok {
			return fmt.Errorf("%w: skill attachment is managed by the plugin preset", ErrConflict)
		}
	}
	for _, name := range servers {
		if _, ok := a.MCP[name]; ok {
			return fmt.Errorf("%w: MCP attachment is managed by the plugin preset", ErrConflict)
		}
	}
	return nil
}
