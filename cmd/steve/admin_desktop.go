package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/runtime"
)

func (a *fleetAdmin) DesktopStatus(ctx context.Context) (consoleapi.DesktopStatus, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	configMu.RLock()
	defer configMu.RUnlock()
	return a.desktopStatusLocked(), nil
}

func (a *fleetAdmin) desktopStatusLocked() consoleapi.DesktopStatus {
	if !desktop.IsManagedConfig(a.path) {
		return consoleapi.DesktopStatus{}
	}
	status := consoleapi.DesktopStatus{Enabled: true, NodeID: a.cfg.Gateway.HubID,
		SetupRequired: len(a.cfg.Agents) == 0, AgentCount: len(a.cfg.Agents)}
	for id, item := range a.cfg.Agents {
		if item.Default {
			status.DefaultAgent = id
			break
		}
	}
	return status
}

func (a *fleetAdmin) DesktopDiscover(ctx context.Context) (consoleapi.DesktopDiscovery, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopDiscovery{}, err
	}
	if !desktop.IsManagedConfig(a.path) {
		return consoleapi.DesktopDiscovery{}, fmt.Errorf("本机 Agent 发现仅在桌面 App 中提供")
	}
	candidates := desktop.DiscoverAgents(desktop.DiscoveryOptions{})
	configMu.RLock()
	defer configMu.RUnlock()
	result := consoleapi.DesktopDiscovery{Agents: make([]consoleapi.DesktopAgentCandidate, 0, len(candidates))}
	for _, item := range candidates {
		result.Agents = append(result.Agents, consoleapi.DesktopAgentCandidate{
			ID: item.ID, Name: item.Name, Harness: item.Harness, Executable: item.Executable,
			Installed: item.Installed, Requires: item.Requires, Registered: localHarnessRegistered(a.cfg.Agents, item.Harness),
		})
	}
	return result, nil
}

func localHarnessRegistered(agents map[string]config.Agent, harnessID string) bool {
	for _, item := range agents {
		if item.Node == "" && item.Harness == harnessID {
			return true
		}
	}
	return false
}

func (a *fleetAdmin) DesktopEnroll(ctx context.Context, req consoleapi.DesktopEnrollRequest) (consoleapi.DesktopStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if !desktop.IsManagedConfig(a.path) {
		return consoleapi.DesktopStatus{}, fmt.Errorf("本机 Agent 注册仅在桌面 App 中提供")
	}
	if len(req.AgentIDs) == 0 {
		return consoleapi.DesktopStatus{}, fmt.Errorf("请选择要注册的本机 Agent")
	}
	if a.manager == nil || a.catalog == nil {
		return consoleapi.DesktopStatus{}, fmt.Errorf("本机 Agent 服务尚未就绪，请稍后重试")
	}
	// Only identifiers cross the API. Re-discover paths on this machine so
	// a stale selection or a client-supplied command cannot be registered.
	candidates := make(map[string]desktop.AgentCandidate)
	for _, item := range desktop.DiscoverAgents(desktop.DiscoveryOptions{}) {
		candidates[item.ID] = item
	}
	configMu.RLock()
	agents := make(map[string]config.Agent, len(a.cfg.Agents))
	maps.Copy(agents, a.cfg.Agents)
	harnesses := maps.Clone(a.cfg.Harnesses)
	statePath := a.cfg.Gateway.StatePath
	configMu.RUnlock()
	addedAgents := make(map[string]config.Agent)
	addedHarnesses := make(map[string]config.Harness)
	var selected, agentIDs []string
	seen := make(map[string]bool, len(req.AgentIDs))
	for _, id := range req.AgentIDs {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		seen[id] = true
		item, ok := candidates[id]
		if !ok {
			return consoleapi.DesktopStatus{}, fmt.Errorf("不支持的本机 Agent %q，请刷新后重新选择", id)
		}
		if localHarnessRegistered(agents, item.Harness) {
			continue
		}
		if _, exists := agents[id]; exists {
			return consoleapi.DesktopStatus{}, fmt.Errorf("Agent 名称 %s 已被占用，请在资源中检查现有 Agent", id)
		}
		registered, tool, err := desktop.Registration(item)
		if err != nil {
			return consoleapi.DesktopStatus{}, err
		}
		if configured, exists := harnesses[item.Harness]; exists {
			if configured.Adapter != tool.Adapter || tool.Adapter == "" && (configured.Command != tool.Command || !slices.Equal(configured.Args, tool.Args)) {
				return consoleapi.DesktopStatus{}, fmt.Errorf("%s 的现有工具配置与本次发现不同，请先在资源中检查配置", item.Name)
			}
		} else {
			addedHarnesses[item.Harness] = tool
		}
		registered.Default = len(agents) == 0
		agents[id] = registered
		addedAgents[id] = registered
		agentIDs = append(agentIDs, id)
		selected = append(selected, item.Harness)
	}
	if len(addedAgents) == 0 {
		return a.DesktopStatus(ctx)
	}
	if _, err := (&config.Config{Agents: agents}).AgentCatalog(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	// Expensive local preparation and adapter installation do not hold the
	// configuration lock. Existing requests and status reads remain usable.
	stateDir := filepath.Dir(statePath)
	if err := runtime.PrepareSelected(stateDir, selected); err != nil {
		return consoleapi.DesktopStatus{}, fmt.Errorf("准备所选 Agent 运行环境：%w", err)
	}
	install := &config.Config{Gateway: config.Gateway{StatePath: statePath}, Harnesses: addedHarnesses}
	if err := install.PrepareAdapters(ctx); err != nil {
		return consoleapi.DesktopStatus{}, fmt.Errorf("安装所选 Agent 适配器：%w", err)
	}
	if a.skills != nil {
		if err := a.skills.AddDests(runtime.SelectedSkillDests(stateDir, selected)...); err != nil {
			return consoleapi.DesktopStatus{}, fmt.Errorf("准备所选 Agent skills：%w", err)
		}
	}
	configMu.Lock()
	defer configMu.Unlock()
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	// Rebuild from the current configuration after preparation, preserving
	// unrelated settings saved while adapters were being installed.
	candidate := *a.cfg
	candidate.Agents = make(map[string]config.Agent, len(a.cfg.Agents)+len(addedAgents))
	maps.Copy(candidate.Agents, a.cfg.Agents)
	candidate.Harnesses = make(map[string]config.Harness, len(a.cfg.Harnesses)+len(addedHarnesses))
	maps.Copy(candidate.Harnesses, a.cfg.Harnesses)
	for _, id := range agentIDs {
		item := addedAgents[id]
		item.Default = len(candidate.Agents) == 0
		candidate.Agents[id] = item
	}
	maps.Copy(candidate.Harnesses, addedHarnesses)
	preparedCatalog, err := candidate.AgentCatalog()
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	preparedManager, err := harnessRuntimeConfig(&candidate).HarnessManager()
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	saveErr := a.persistConfig(&candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		return consoleapi.DesktopStatus{}, saveErr
	}
	a.manager.Publish(preparedManager)
	a.cfg.Agents, a.cfg.Harnesses = candidate.Agents, candidate.Harnesses
	a.catalog.Publish(preparedCatalog)
	log.Printf("steve: local agents registered agents=%s", strings.Join(agentIDs, ","))
	return a.desktopStatusLocked(), saveErr
}
