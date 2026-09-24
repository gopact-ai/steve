package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func (a *Service) nodeForAgentEnrollment(name string) (config.Node, error) {
	if name == "" || name == "hub" {
		return config.Node{}, errors.New("请选择已接入的远端机器；本机工具请在本机登记")
	}
	a.configStore().RLock()
	target, ok := a.Cfg.Nodes[name]
	a.configStore().RUnlock()
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
	a.configStore().RLock()
	defer a.configStore().RUnlock()
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
	a.describeOffers(ctx, name, discovered.Agents)
	return discovered, nil
}

// describeOffers adds what each tool was last seen offering on the machine,
// so the owner can pick a model and a reasoning effort while registering
// rather than going back into settings afterwards. A tool that has never run
// there offers nothing yet, which the page shows as such.
func (a *Service) describeOffers(ctx context.Context, name string, candidates []agenttools.Candidate) {
	if len(candidates) == 0 {
		return
	}
	offers := a.harnessOffers(ctx, name)
	for i := range candidates {
		harness, ok := offers[candidates[i].Harness]
		if !ok {
			continue
		}
		candidates[i].Model = harness.Model
		candidates[i].Models = harness.Models
		for _, selector := range harness.Selectors {
			candidates[i].Selectors = append(candidates[i].Selectors, agenttools.CandidateSelector{ID: selector.ID, Name: selector.Name, Category: selector.Category, Current: selector.Current, Choices: selector.Choices, Values: selector.Values})
		}
	}
}

// harnessOffers is what each tool on one machine was last seen offering.
// An empty machine name reads this computer, which the read model files
// under the hub. Nothing is offered before a tool has run there.
func (a *Service) harnessOffers(ctx context.Context, name string) map[string]readmodel.Harness {
	offers := map[string]readmodel.Harness{}
	if a.View == nil {
		return offers
	}
	for _, item := range a.View.Snapshot(ctx).Nodes {
		if name == "" && item.Role != readmodel.RoleHub || name != "" && item.Name != name {
			continue
		}
		for _, harness := range item.Harnesses {
			offers[harness.ID] = harness
		}
	}
	return offers
}

// EnrollNodeAgent saves the logical Agents only after the selected node has
// installed and atomically published its own declaration. Every requested
// agent is validated before anything is installed, and the coordinator's
// configuration is written once, so a partial batch cannot be saved. If that
// write fails, the installed tools stay available for a safe retry.
func (a *Service) EnrollNodeAgent(ctx context.Context, name string, req agenttools.EnrollRequest) (agenttools.Enrollment, error) {
	requested := req.Requested()
	if len(requested) == 0 {
		return agenttools.Enrollment{}, errors.New("请选择要登记的 Agent")
	}
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return agenttools.Enrollment{}, err
	}
	if a.Catalog == nil {
		return agenttools.Enrollment{}, errors.New("Agent 服务尚未就绪")
	}
	a.configStore().RLock()
	existing := maps.Clone(a.Cfg.Agents)
	a.configStore().RUnlock()
	planned, err := planNodeAgents(name, requested, existing)
	if err != nil {
		return agenttools.Enrollment{}, err
	}
	// Each installation publishes a new node declaration, so the revision the
	// next one must match is the one the previous receipt reported.
	revision := req.ExpectedRevision
	var result agenttools.Enrollment
	var installErr error
	for _, item := range planned.order {
		receipt, err := a.Nodes.EnrollAgent(ctx, name, agenttools.InstallRequest{CandidateID: planned.candidates[item], ExpectedRevision: revision})
		if err != nil && !node.SettingsCommitted(err) {
			return receipt, err
		}
		installErr = errors.Join(installErr, err)
		if receipt.Revision != "" {
			revision = receipt.Revision
		}
		receipt.AgentID = item
		result = receipt
	}
	result.Agents = planned.order
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.configStore().Lock()
	defer a.configStore().Unlock()
	if err := a.checkAgentNodeTarget(name, target); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	candidate := *a.Cfg
	candidate.Agents = make(map[string]config.Agent, len(a.Cfg.Agents)+len(planned.order))
	maps.Copy(candidate.Agents, a.Cfg.Agents)
	added := false
	for _, id := range planned.order {
		if current, ok := candidate.Agents[id]; ok {
			if current.Node != name || current.Harness != planned.agents[id].Harness {
				return result, fmt.Errorf("Agent 名称 %s 已被占用", id)
			}
			continue
		}
		entry := planned.agents[id]
		entry.Default = len(candidate.Agents) == 0
		candidate.Agents[id] = entry
		added = true
	}
	if !added {
		result.Registered = true
		return result, installErr
	}
	applyDefault(candidate.Agents, planned.preferred, planned.order)
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
	slog.Info(fmt.Sprintf("steve: selected agents registered agents=%s node=%s", strings.Join(planned.order, ","), name), "node", name)
	return result, errors.Join(installErr, saveErr)
}

// nodeAgentPlan is the validated batch: the config entry for each agent, the
// discovered tool it came from, the order the owner chose and the default.
type nodeAgentPlan struct {
	agents     map[string]config.Agent
	candidates map[string]string
	order      []string
	preferred  string
}

// planNodeAgents checks every requested agent against the supported tools and
// the names already in use, before anything is installed on the machine.
func planNodeAgents(node string, requested []agenttools.EnrollAgent, existing map[string]config.Agent) (nodeAgentPlan, error) {
	plan := nodeAgentPlan{agents: map[string]config.Agent{}, candidates: map[string]string{}}
	seen := map[string]bool{}
	for _, want := range requested {
		canonical, ok := agenttools.Lookup(want.CandidateID)
		if !ok {
			return nodeAgentPlan{}, agenttools.ErrUnsupported
		}
		id := strings.ToLower(strings.TrimSpace(want.AgentID))
		if id == "" {
			id = want.CandidateID
		}
		if !NameShape.MatchString(id) {
			return nodeAgentPlan{}, fmt.Errorf("Agent 名 %q 只能是小写字母、数字、点、下划线、连字符", want.AgentID)
		}
		if seen[id] {
			return nodeAgentPlan{}, fmt.Errorf("Agent 名称 %s 在这次登记里出现了两次", id)
		}
		seen[id] = true
		if current, ok := existing[id]; ok && (current.Node != node || current.Harness != canonical.Harness) {
			return nodeAgentPlan{}, fmt.Errorf("Agent 名称 %s 已被占用", id)
		}
		entry := config.Agent{Node: node, Harness: canonical.Harness, About: strings.TrimSpace(want.About), Model: strings.TrimSpace(want.Model)}
		if len(want.Options) > 0 {
			entry.Options = maps.Clone(want.Options)
		}
		plan.agents[id] = entry
		plan.candidates[id] = want.CandidateID
		plan.order = append(plan.order, id)
		if want.Default {
			plan.preferred = id
		}
	}
	return plan, nil
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
