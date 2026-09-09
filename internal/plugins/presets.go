package plugins

import "maps"

// AgentOrigin records the exact template applied to a user-owned Agent.
// Upgrades can preserve fields changed since this baseline.
type AgentOrigin struct {
	Adopted      *Adoption      `json:"adopted,omitempty"`
	CommandID    string         `json:"command_id,omitempty"`
	Applied      *AppliedPreset `json:"applied,omitempty"`
	Installation string         `json:"installation"`
	Preset       string         `json:"preset"`
	Digest       string         `json:"digest"`
	Template     AgentPreset    `json:"template"`
}

func (origin *AgentOrigin) Clone() *AgentOrigin {
	if origin == nil {
		return nil
	}
	copy := *origin
	copy.Adopted = origin.Adopted.Clone()
	if origin.Applied != nil {
		applied := *origin.Applied
		applied.Skills = append([]string(nil), applied.Skills...)
		applied.MCPServers = append([]string(nil), applied.MCPServers...)
		applied.Options = maps.Clone(applied.Options)
		applied.Preserved = append([]string(nil), applied.Preserved...)
		copy.Applied = &applied
	}
	copy.Template.Options = maps.Clone(origin.Template.Options)
	copy.Template.Skills = append([]string(nil), origin.Template.Skills...)
	copy.Template.MCPServers = append([]string(nil), origin.Template.MCPServers...)
	return &copy
}

type AppliedPreset struct {
	Skills       []string          `json:"skills,omitempty"`
	MCPServers   []string          `json:"mcp_servers,omitempty"`
	AgentID      string            `json:"agent_id"`
	Revision     string            `json:"revision"`
	Node         string            `json:"node"`
	Harness      string            `json:"harness"`
	Model        string            `json:"model,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Preserved    []string          `json:"preserved"`
}
