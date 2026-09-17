package admin

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/runtime"
)

func (a *Service) DesktopStatus(ctx context.Context) (consoleapi.DesktopStatus, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	return a.desktopStatusLocked(), nil
}

func (a *Service) desktopStatusLocked() consoleapi.DesktopStatus {
	if !desktop.IsManagedConfig(a.Path) {
		return consoleapi.DesktopStatus{}
	}
	stateDir := filepath.Dir(a.Path)
	status := consoleapi.DesktopStatus{Enabled: true, NodeID: a.Cfg.Gateway.HubID, AgentCount: len(a.Cfg.Agents),
		WorkspacePath: a.Cfg.Projects[config.DefaultProjectID(a.Cfg.Gateway.DefaultProject, a.Cfg.Projects)].Home.Path}
	status.WorkspaceManaged = desktop.ManagedWorkspace(stateDir, status.WorkspacePath)
	for id, item := range a.Cfg.Agents {
		if item.Default {
			status.DefaultAgent = id
			break
		}
	}
	progress, err := desktop.ReadSetup(stateDir)
	if err != nil {
		slog.Warn("desktop: setup progress unreadable, reopening the guide", "error", err)
		progress = desktop.SetupProgress{Step: desktop.SetupSteps[0]}
	}
	status.Setup = &consoleapi.DesktopSetup{Step: progress.Step, Done: progress.Done}
	status.SetupRequired = !progress.Done
	return status
}

// DesktopSetup records the guide page to open next.
func (a *Service) DesktopSetup(ctx context.Context, req consoleapi.DesktopSetupRequest) (consoleapi.DesktopStatus, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if !desktop.IsManagedConfig(a.Path) {
		return consoleapi.DesktopStatus{}, fmt.Errorf("新手引导仅在桌面 App 中提供")
	}
	if err := desktop.SaveSetup(filepath.Dir(a.Path), desktop.SetupProgress{Step: req.Step, Done: req.Done}); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	return a.DesktopStatus(ctx)
}

// DesktopWorkspace moves the default project to a directory the owner chose,
// creating it when needed.
func (a *Service) DesktopWorkspace(ctx context.Context, req consoleapi.DesktopWorkspaceRequest) (consoleapi.DesktopStatus, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if !desktop.IsManagedConfig(a.Path) {
		return consoleapi.DesktopStatus{}, fmt.Errorf("工作目录设置仅在桌面 App 中提供")
	}
	ConfigMu.RLock()
	id := config.DefaultProjectID(a.Cfg.Gateway.DefaultProject, a.Cfg.Projects)
	home := a.Cfg.Projects[id].Home
	local := a.Cfg.LocalHomeNode(home.Node)
	ConfigMu.RUnlock()
	if err := desktop.CheckWorkspaceProject(id, local, home.Node); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	path, err := desktop.PrepareWorkspace(req.Path, filepath.Dir(a.Path))
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if err := a.SetProjectHome(ctx, id, path); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	return a.DesktopStatus(ctx)
}

func (a *Service) DesktopDiscover(ctx context.Context) (consoleapi.DesktopDiscovery, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopDiscovery{}, err
	}
	if !desktop.IsManagedConfig(a.Path) {
		return consoleapi.DesktopDiscovery{}, fmt.Errorf("本机 Agent 发现仅在桌面 App 中提供")
	}
	candidates := desktop.DiscoverAgents(desktop.DiscoveryOptions{})
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	result := consoleapi.DesktopDiscovery{Agents: make([]consoleapi.DesktopAgentCandidate, 0, len(candidates))}
	for _, item := range candidates {
		result.Agents = append(result.Agents, consoleapi.DesktopAgentCandidate{
			ID: item.ID, Name: item.Name, Harness: item.Harness, Executable: item.Executable,
			Installed: item.Installed, Requires: item.Requires, Registered: localHarnessRegistered(a.Cfg.Agents, item.Harness),
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

// applyDefault leaves exactly one agent marked as the default: the one the
// owner chose, or the first agent registered here when nothing held it yet.
// A catalog without a default is refused, so this runs on every candidate
// map before it is validated.
func applyDefault(agents map[string]config.Agent, preferred string, added []string) {
	if preferred == "" {
		for _, item := range agents {
			if item.Default {
				return
			}
		}
		if len(added) == 0 {
			return
		}
		preferred = added[0]
	}
	for id, item := range agents {
		item.Default = id == preferred
		agents[id] = item
	}
}

func (a *Service) DesktopEnroll(ctx context.Context, req consoleapi.DesktopEnrollRequest) (consoleapi.DesktopStatus, error) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if !desktop.IsManagedConfig(a.Path) {
		return consoleapi.DesktopStatus{}, fmt.Errorf("本机 Agent 注册仅在桌面 App 中提供")
	}
	requested := req.Agents
	if len(requested) == 0 {
		for _, id := range req.AgentIDs {
			requested = append(requested, consoleapi.DesktopEnrollAgent{CandidateID: id})
		}
	}
	if len(requested) == 0 {
		return consoleapi.DesktopStatus{}, fmt.Errorf("请选择要注册的本机 Agent")
	}
	if a.Manager == nil || a.Catalog == nil {
		return consoleapi.DesktopStatus{}, fmt.Errorf("本机 Agent 服务尚未就绪，请稍后重试")
	}
	// Only identifiers cross the API. Re-discover paths on this machine so
	// a stale selection or a client-supplied command cannot be registered.
	candidates := make(map[string]desktop.AgentCandidate)
	for _, item := range desktop.DiscoverAgents(desktop.DiscoveryOptions{}) {
		candidates[item.ID] = item
	}
	ConfigMu.RLock()
	agents := make(map[string]config.Agent, len(a.Cfg.Agents))
	maps.Copy(agents, a.Cfg.Agents)
	harnesses := maps.Clone(a.Cfg.Harnesses)
	statePath := a.Cfg.Gateway.StatePath
	ConfigMu.RUnlock()
	addedAgents := make(map[string]config.Agent)
	addedHarnesses := make(map[string]config.Harness)
	var selected, agentIDs []string
	// preferred is the agent the owner marked as the default. It is only
	// honoured once that agent has actually been registered here.
	var preferred string
	seen := make(map[string]bool, len(requested))
	for _, want := range requested {
		candidateID := strings.TrimSpace(want.CandidateID)
		if seen[candidateID] {
			continue
		}
		seen[candidateID] = true
		item, ok := candidates[candidateID]
		if !ok {
			return consoleapi.DesktopStatus{}, fmt.Errorf("不支持的本机 Agent %q，请刷新后重新选择", candidateID)
		}
		name := strings.ToLower(strings.TrimSpace(want.AgentID))
		if name == "" {
			name = candidateID
		}
		if !NameShape.MatchString(name) {
			return consoleapi.DesktopStatus{}, fmt.Errorf("Agent 名 %q 只能是小写字母、数字、点、下划线、连字符", name)
		}
		if localHarnessRegistered(agents, item.Harness) {
			continue
		}
		if _, exists := agents[name]; exists {
			return consoleapi.DesktopStatus{}, fmt.Errorf("Agent 名称 %s 已被占用，请换一个名字", name)
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
		// A renamed agent does not also squat the tool's own ID, so that
		// name stays free for another agent later.
		if name != candidateID {
			registered.Aliases = nil
		}
		registered.About = strings.TrimSpace(want.About)
		agents[name] = registered
		addedAgents[name] = registered
		agentIDs = append(agentIDs, name)
		selected = append(selected, item.Harness)
		if want.Default {
			preferred = name
		}
	}
	if len(addedAgents) == 0 {
		return a.DesktopStatus(ctx)
	}
	applyDefault(agents, preferred, agentIDs)
	if _, err := (&config.Config{Agents: agents}).AgentCatalog(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	if err := a.prepareDesktopAgents(ctx, statePath, selected, addedHarnesses); err != nil {
		return consoleapi.DesktopStatus{}, err
	}

	ConfigMu.Lock()
	defer ConfigMu.Unlock()
	if err := ctx.Err(); err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	// Rebuild from the current configuration after preparation, preserving
	// unrelated settings saved while adapters were being installed.
	candidate := *a.Cfg
	candidate.Agents = make(map[string]config.Agent, len(a.Cfg.Agents)+len(addedAgents))
	maps.Copy(candidate.Agents, a.Cfg.Agents)
	candidate.Harnesses = make(map[string]config.Harness, len(a.Cfg.Harnesses)+len(addedHarnesses))
	maps.Copy(candidate.Harnesses, a.Cfg.Harnesses)
	for _, id := range agentIDs {
		candidate.Agents[id] = addedAgents[id]
	}
	applyDefault(candidate.Agents, preferred, agentIDs)
	maps.Copy(candidate.Harnesses, addedHarnesses)
	preparedCatalog, err := candidate.AgentCatalog()
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	preparedManager, err := HarnessRuntimeConfig(&candidate).HarnessManager()
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	saveErr := a.PersistConfig(&candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		return consoleapi.DesktopStatus{}, saveErr
	}
	a.Manager.Publish(preparedManager)
	a.Cfg.Agents, a.Cfg.Harnesses = candidate.Agents, candidate.Harnesses
	a.Catalog.Publish(preparedCatalog)
	slog.Info(fmt.Sprintf("steve: local agents registered agents=%s", strings.Join(agentIDs, ",")))
	return a.desktopStatusLocked(), saveErr
}

func (a *Service) prepareDesktopAgents(ctx context.Context, statePath string, selected []string, harnesses map[string]config.Harness) error {
	// Expensive local preparation and adapter installation do not hold the
	// configuration lock. Existing requests and status reads remain usable.
	stateDir := filepath.Dir(statePath)
	if err := runtime.PrepareSelected(stateDir, selected); err != nil {
		return fmt.Errorf("准备所选 Agent 运行环境：%w", err)
	}
	install := &config.Config{Gateway: config.Gateway{StatePath: statePath}, Harnesses: harnesses}
	if err := install.PrepareAdapters(ctx); err != nil {
		return fmt.Errorf("安装所选 Agent 适配器：%w", err)
	}
	if a.LiveSkills != nil {
		if err := a.LiveSkills.AddDests(runtime.SelectedSkillDests(stateDir, selected)...); err != nil {
			return fmt.Errorf("准备所选 Agent skills：%w", err)
		}
	}
	return nil
}
