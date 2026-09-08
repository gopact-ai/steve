package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (a *Service) nodeForAgentEnrollment(name string) (config.Node, error) {
	if name == "" || name == "hub" {
		return config.Node{}, errors.New("请选择已接入的远端机器；本机工具请在本机登记")
	}
	ConfigMu.RLock()
	target, ok := a.Cfg.Nodes[name]
	ConfigMu.RUnlock()
	if !ok {
		return config.Node{}, fmt.Errorf("没有叫 %q 的机器", name)
	}
	if a.Nodes == nil {
		return config.Node{}, errors.New("远端机器服务尚未就绪")
	}
	return target, nil
}

// The caller holds configMu while comparing the authenticated target with the
// declaration it is about to commit. Reusing a display name cannot reuse an
// earlier machine's discovery or installation result.
func (a *Service) checkAgentNodeTarget(name string, expected config.Node) error {
	current, ok := a.Cfg.Nodes[name]
	if !ok || current.Addr != expected.Addr || current.Token != expected.Token {
		return fmt.Errorf("%w: 机器身份或连接已变化，请重新选择", nodewire.ErrSettingsRevisionConflict)
	}
	return nil
}

// NodeAgents asks the selected machine for its own installed tools. SSH
// previews and commands discovered on the coordinator are never used here.
func (a *Service) NodeAgents(ctx context.Context, name string) (agenttools.Discovery, error) {
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return agenttools.Discovery{}, err
	}
	discovered, err := a.Nodes.AgentTools(ctx, name)
	if err != nil {
		return agenttools.Discovery{}, err
	}
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	if err := a.checkAgentNodeTarget(name, target); err != nil {
		return agenttools.Discovery{}, err
	}
	for i := range discovered.Agents {
		for _, item := range a.Cfg.Agents {
			if item.Node == name && item.Harness == discovered.Agents[i].Harness {
				discovered.Agents[i].Registered = true
				break
			}
		}
	}
	return discovered, nil
}

// EnrollNodeAgent saves the logical Agent only after the selected node has
// installed and atomically published its own declaration. If the coordinator
// file cannot be saved, the installed tool stays available for a safe retry.
func (a *Service) EnrollNodeAgent(ctx context.Context, name string, req agenttools.EnrollRequest) (agenttools.Enrollment, error) {
	id := strings.ToLower(strings.TrimSpace(req.AgentID))
	if !NameShape.MatchString(id) {
		return agenttools.Enrollment{}, errors.New("Agent 名只能是小写字母、数字、点、下划线、连字符")
	}
	canonical, ok := agenttools.Lookup(req.CandidateID)
	if !ok {
		return agenttools.Enrollment{}, agenttools.ErrUnsupported
	}
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return agenttools.Enrollment{}, err
	}
	ConfigMu.RLock()
	existing, exists := a.Cfg.Agents[id]
	ConfigMu.RUnlock()
	if exists && (existing.Node != name || existing.Harness != canonical.Harness) {
		return agenttools.Enrollment{}, fmt.Errorf("Agent 名称 %s 已被占用", id)
	}
	if a.Catalog == nil {
		return agenttools.Enrollment{}, errors.New("Agent 服务尚未就绪")
	}
	result, installErr := a.Nodes.EnrollAgent(ctx, name, agenttools.InstallRequest{CandidateID: req.CandidateID, ExpectedRevision: req.ExpectedRevision})
	if installErr != nil && !node.SettingsCommitted(installErr) {
		return result, installErr
	}
	result.AgentID = id
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ConfigMu.Lock()
	defer ConfigMu.Unlock()
	if err := a.checkAgentNodeTarget(name, target); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if existing, ok := a.Cfg.Agents[id]; ok {
		if existing.Node != name || existing.Harness != canonical.Harness {
			return result, fmt.Errorf("Agent 名称 %s 已被占用", id)
		}
		result.Registered = true
		return result, installErr
	}
	candidate := *a.Cfg
	candidate.Agents = make(map[string]config.Agent, len(a.Cfg.Agents)+1)
	for name, item := range a.Cfg.Agents {
		candidate.Agents[name] = item
	}
	candidate.Agents[id] = config.Agent{Node: name, Harness: canonical.Harness, Default: len(candidate.Agents) == 0}
	prepared, err := candidate.AgentCatalog()
	if err != nil {
		return result, err
	}
	saveErr := a.PersistConfig(&candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		return result, saveErr
	}
	a.Cfg.Agents = candidate.Agents
	a.Catalog.Publish(prepared)
	result.Registered = true
	slog.Info(fmt.Sprintf("steve: selected agent registered agent=%s node=%s harness=%s", id, name, canonical.Harness), "agent", id, "node", name, "harness", canonical.Harness)
	return result, errors.Join(installErr, saveErr)
}

func (a *Service) checkRemoteHarness(ctx context.Context, nodeID, harnessID string) (config.Node, error) {
	if nodeID == "" {
		return config.Node{}, nil
	}
	target, err := a.nodeForAgentEnrollment(nodeID)
	if err != nil {
		return config.Node{}, err
	}
	settings, err := a.Nodes.Settings(ctx, nodeID)
	if err != nil {
		return config.Node{}, err
	}
	declared, ok := settings.Harnesses[harnessID]
	if !ok || strings.TrimSpace(declared.Command) == "" {
		return config.Node{}, fmt.Errorf("机器 %s 尚未登记 AI 工具 %s，请先选择该机器已安装的工具", nodeID, harnessID)
	}
	// Same-process enrollment updates the worker without touching this
	// registry's cached advert. Publish the tool before the Agent can start
	// its first session; do not wait for the periodic capability refresh.
	advert, err := a.Nodes.Refresh(ctx, nodeID)
	if err != nil {
		return config.Node{}, err
	}
	for _, offered := range advert.Harnesses {
		if offered.ID == harnessID {
			return target, nil
		}
	}
	return config.Node{}, fmt.Errorf("机器 %s 尚未报告 AI 工具 %s，请刷新后重试", nodeID, harnessID)
}
