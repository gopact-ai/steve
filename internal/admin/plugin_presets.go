package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func agentRevision(agents map[string]config.Agent) string {
	raw, _ := json.Marshal(agents)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func pluginAgentView(id string, item config.Agent) consoleapi.PluginAgentView {
	return consoleapi.PluginAgentView{Skills: slices.Clone(item.Skills), MCPServers: slices.Clone(item.MCPServers), ID: id, Node: item.Node, Harness: item.Harness, Model: item.Model, Options: maps.Clone(item.Options), SystemPrompt: item.SystemPrompt, Origin: item.PluginOrigin.Clone()}
}

func (s *PluginService) PreviewPluginPreset(ctx context.Context, id string, req consoleapi.PluginPresetRequest) (consoleapi.PluginPresetPreview, error) {
	ConfigMu.RLock()
	cfg := *s.Admin.Cfg
	cfg.Plugins = config.ClonePluginInstallations(cfg.Plugins)
	cfg.Agents = maps.Clone(cfg.Agents)
	ConfigMu.RUnlock()
	return s.presetPreview(ctx, id, req, &cfg)
}

func (s *PluginService) presetPreview(ctx context.Context, id string, req consoleapi.PluginPresetRequest, cfg *config.Config) (consoleapi.PluginPresetPreview, error) {
	if !NameShape.MatchString(req.AgentID) {
		return consoleapi.PluginPresetPreview{}, errors.New("agent ID is invalid")
	}
	item, exists := cfg.Plugins[id]
	if !exists || item.Digest != req.Digest {
		return consoleapi.PluginPresetPreview{}, plugins.ErrConflict
	}
	node := req.Node
	if s.Admin.ClusterMode {
		node = s.Admin.nodeKey(node)
	}
	if _, exists := item.Targets[node]; !exists {
		return consoleapi.PluginPresetPreview{}, errors.New("agent must use a configured plugin target")
	}
	if len(item.Projects) == 0 {
		return consoleapi.PluginPresetPreview{}, plugins.ErrInvalid
	}
	record, err := s.Library.Record(ctx, item.Projects[0], item.Digest)
	if err != nil {
		return consoleapi.PluginPresetPreview{}, err
	}
	template, exists := record.Manifest.Agents[req.Preset]
	if !exists {
		return consoleapi.PluginPresetPreview{}, errors.New("plugin preset is unknown")
	}
	candidate := config.Agent{Harness: template.Harness, Node: node, Model: template.Model, Options: maps.Clone(template.Options), SystemPrompt: template.SystemPrompt, Default: len(cfg.Agents) == 0}
	view := consoleapi.PluginPresetPreview{Revision: agentRevision(cfg.Agents), Preserved: []string{}}
	if req.Adopt != nil {
		if _, found := cfg.Agents[req.AgentID]; !found {
			return view, errors.New("adoption requires an existing Agent")
		}
	}
	if existing, found := cfg.Agents[req.AgentID]; found {
		shown := pluginAgentView(req.AgentID, existing)
		view.Existing = &shown
		var err error
		candidate, view.Preserved, err = mergePresetAgent(existing, id, req, template)
		if err != nil {
			return view, err
		}
		if candidate.Node != node {
			return view, errors.New("changing an existing preset agent's node requires its agent settings")
		}
	}
	if req.Model != nil {
		candidate.Model = *req.Model
	}
	if req.SystemPrompt != nil {
		candidate.SystemPrompt = *req.SystemPrompt
	}
	if req.Options != nil {
		candidate.Options = maps.Clone(req.Options)
	}
	var adopted *plugins.Adoption
	if candidate.PluginOrigin != nil {
		adopted = candidate.PluginOrigin.Adopted.Clone()
	}
	if req.Adopt != nil {
		adopted = req.Adopt.Clone()
	}
	candidate.PluginOrigin = &plugins.AgentOrigin{Adopted: adopted, Installation: id, Preset: req.Preset, Digest: item.Digest, Template: template}
	view.Proposed = pluginAgentView(req.AgentID, candidate)
	return view, nil
}

func (s *PluginService) ApplyPluginPreset(ctx context.Context, id string, req consoleapi.PluginPresetRequest) (result consoleapi.PluginPresetPreview, err error) {
	if req.CommandID == "" || req.BaseRevision == "" {
		return consoleapi.PluginPresetPreview{}, plugins.ErrInvalid
	}
	operation, err := s.Library.BeginOperation(ctx, req.CommandID, "preset", struct {
		Installation string
		Request      consoleapi.PluginPresetRequest
	}{id, req})
	if err != nil {
		return consoleapi.PluginPresetPreview{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, s.Library.FinishOperation(cleanup, operation, err))
	}()
	// The immutable result survives response loss and later unrelated edits.
	var saved consoleapi.PluginPresetPreview
	if found, err := s.Library.Ledger.GetBinding(ctx, "plugin-preset-result", req.CommandID, &saved); err != nil {
		return saved, err
	} else if found {
		return saved, s.Library.FinishOperation(ctx, operation, nil)
	}
	ConfigMu.RLock()
	existing := s.Admin.Cfg.Agents[req.AgentID].PluginOrigin.Clone()
	ConfigMu.RUnlock()
	if existing != nil && existing.CommandID == req.CommandID && existing.Applied != nil {
		restored := presetResult(existing)
		if err := s.Library.Ledger.PutBinding(ctx, "plugin-preset-result", req.CommandID, restored); err != nil {
			return restored, err
		}
		return restored, s.Library.FinishOperation(ctx, operation, nil)
	}
	preview, err := s.PreviewPluginPreset(ctx, id, req)
	if err != nil {
		return preview, err
	}
	target, err := s.Admin.checkRemoteHarness(ctx, preview.Proposed.Node, preview.Proposed.Harness)
	if err != nil {
		return preview, err
	}
	err = s.Admin.changeAgents(func(agents map[string]config.Agent) error {
		if agentRevision(agents) != req.BaseRevision {
			return consoleapi.ErrSettingsConflict
		}
		current, err := s.presetPreview(ctx, id, req, s.Admin.Cfg)
		if err != nil {
			return err
		}
		preview = current
		if current.Proposed.Node == "" {
			if _, ok := s.Admin.Cfg.Harnesses[current.Proposed.Harness]; !ok {
				return errors.New("preset harness is not registered")
			}
		} else {
			if err := s.Admin.checkAgentNodeTarget(current.Proposed.Node, target); err != nil {
				return err
			}
		}
		candidate, exists := agents[req.AgentID]
		if !exists {
			candidate.Default = len(agents) == 0
		}
		candidate.Harness = current.Proposed.Harness
		candidate.Node = current.Proposed.Node
		candidate.Model = current.Proposed.Model
		candidate.Options = maps.Clone(current.Proposed.Options)
		candidate.SystemPrompt = current.Proposed.SystemPrompt
		candidate.Skills = slices.Clone(current.Proposed.Skills)
		candidate.MCPServers = slices.Clone(current.Proposed.MCPServers)
		candidate.PluginOrigin = current.Proposed.Origin.Clone()
		candidate.PluginOrigin.CommandID = req.CommandID
		candidate.PluginOrigin.Applied = &plugins.AppliedPreset{Skills: slices.Clone(candidate.Skills), MCPServers: slices.Clone(candidate.MCPServers), AgentID: req.AgentID, Revision: req.BaseRevision, Node: candidate.Node, Harness: candidate.Harness, Model: candidate.Model, Options: maps.Clone(candidate.Options), SystemPrompt: candidate.SystemPrompt, Preserved: append([]string{}, current.Preserved...)}
		preview = presetResult(candidate.PluginOrigin)
		agents[req.AgentID] = candidate
		return nil
	})
	if err != nil && !config.Committed(err) {
		return preview, err
	}
	if err != nil {
		preview.Warning = err.Error()
	}
	if err := s.Library.Ledger.PutBinding(ctx, "plugin-preset-result", req.CommandID, preview); err != nil {
		return preview, err
	}
	return preview, s.Library.FinishOperation(ctx, operation, nil)
}

func presetResult(origin *plugins.AgentOrigin) consoleapi.PluginPresetPreview {
	value := origin.Applied
	return consoleapi.PluginPresetPreview{Revision: value.Revision, Proposed: consoleapi.PluginAgentView{Skills: slices.Clone(value.Skills), MCPServers: slices.Clone(value.MCPServers), ID: value.AgentID, Node: value.Node, Harness: value.Harness, Model: value.Model, Options: maps.Clone(value.Options), SystemPrompt: value.SystemPrompt, Origin: origin.Clone()}, Preserved: append([]string{}, value.Preserved...)}
}
