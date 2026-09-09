package admin

import (
	"errors"
	"maps"
	"slices"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func mergePresetAgent(existing config.Agent, installation string, req consoleapi.PluginPresetRequest, template plugins.AgentPreset) (config.Agent, []string, error) {
	candidate := existing
	candidate.PluginOrigin = existing.PluginOrigin.Clone()
	candidate.Options = maps.Clone(existing.Options)
	if req.Adopt != nil {
		if existing.PluginOrigin != nil {
			return candidate, nil, errors.New("agent already has a plugin origin")
		}
		if err := req.Adopt.Check(template, existing.Skills, existing.MCPServers); err != nil {
			return candidate, nil, err
		}
		for name := range req.Adopt.MCP {
			if slices.Contains(existing.Requires, "mcp:"+name) {
				return candidate, nil, errors.New("remove the adopted legacy MCP from Agent requirements before migration")
			}
		}
		candidate.Skills, candidate.MCPServers = req.Adopt.Remaining(existing.Skills, existing.MCPServers)
		return candidate, []string{"model", "options", "system_prompt", "harness"}, nil
	}
	if existing.PluginOrigin == nil || existing.PluginOrigin.Installation != installation || existing.PluginOrigin.Preset != req.Preset {
		return candidate, nil, errors.New("agent ID belongs to user configuration or another preset")
	}
	previous := existing.PluginOrigin.Template
	preserved := []string{}
	if existing.Model == previous.Model {
		candidate.Model = template.Model
	} else {
		preserved = append(preserved, "model")
	}
	if existing.SystemPrompt == previous.SystemPrompt {
		candidate.SystemPrompt = template.SystemPrompt
	} else {
		preserved = append(preserved, "system_prompt")
	}
	if existing.Harness == previous.Harness {
		candidate.Harness = template.Harness
	} else {
		preserved = append(preserved, "harness")
	}
	keys := map[string]bool{}
	for key := range previous.Options {
		keys[key] = true
	}
	for key := range template.Options {
		keys[key] = true
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	slices.Sort(names)
	if candidate.Options == nil {
		candidate.Options = map[string]string{}
	}
	for _, key := range names {
		current, currentOK := existing.Options[key]
		was, wasOK := previous.Options[key]
		if current == was && currentOK == wasOK {
			if next, ok := template.Options[key]; ok {
				candidate.Options[key] = next
			} else {
				delete(candidate.Options, key)
			}
		} else {
			preserved = append(preserved, "options."+key)
		}
	}
	return candidate, preserved, nil
}
