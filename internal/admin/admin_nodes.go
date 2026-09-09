package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/runtime"
)

// NodeSettings reads what a machine offers: the hub's own from its
// config, a node's from the node.
func (a *Service) NodeSettings(ctx context.Context, name string) (nodewire.Settings, error) {
	if name == NodeName() && !a.ClusterMode {
		return a.hubSettings(), nil
	}
	return a.Nodes.Settings(ctx, name)
}

// SetNodeSettings rewrites what a machine offers and answers what is in
// force: on the hub, the config file and the running manager, assembler,
// roster and launch probe; on a node, the node itself.
func (a *Service) SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if name != NodeName() || a.ClusterMode {
		return a.Nodes.Configure(ctx, name, set)
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.setNodeSettingsLocked(ctx, name, set)
}

// setNodeSettingsLocked is SetNodeSettings for the hub with a.mu held.
func (a *Service) setNodeSettingsLocked(_ context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if set.Revision == "" || set.Revision != a.hubSettings().Revision {
		return nodewire.Settings{}, nodewire.ErrSettingsRevisionConflict
	}
	set = nodewire.CloneSettings(set)
	harnesses, err := a.hubHarnessSettings(set.Harnesses)
	if err != nil {
		return nodewire.Settings{}, err
	}
	servers, err := a.hubMCPSettings(set.MCPServers)
	if err != nil {
		return nodewire.Settings{}, err
	}

	for _, d := range set.Declares {
		if !strings.Contains(d, ":") {
			return nodewire.Settings{}, fmt.Errorf("声明 %q 要写成 kind:id，如 network:office", d)
		}
	}
	ConfigMu.Lock()
	// Agents keep naming harnesses that exist.
	for id, ag := range a.Cfg.Agents {
		if ag.Node == "" {
			if _, ok := harnesses[ag.Harness]; !ok {
				ConfigMu.Unlock()
				return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 AI 工具 %s，不能删", id, ag.Harness)
			}
			for _, srv := range ag.MCPServers {
				if _, ok := servers[srv]; !ok {
					ConfigMu.Unlock()
					return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 MCP 服务器 %s，不能删", id, srv)
				}
			}
		}
	}
	old := *a.Cfg
	a.Cfg.Harnesses = harnesses
	a.Cfg.MCPServers = servers
	a.Cfg.Gateway.Tools = append([]string(nil), set.Tools...)
	a.Cfg.Gateway.Declares = append([]string(nil), set.Declares...)
	a.Cfg.Gateway.Capabilities = append([]string(nil), set.Capabilities...)
	saveErr := a.PersistConfig(a.Cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		a.Cfg.Harnesses, a.Cfg.MCPServers, a.Cfg.Gateway = old.Harnesses, old.MCPServers, old.Gateway
		ConfigMu.Unlock()
		if errors.Is(saveErr, config.ErrFileChanged) {
			return nodewire.Settings{}, errors.Join(nodewire.ErrSettingsRevisionConflict, saveErr)
		}
		return nodewire.Settings{}, saveErr
	}
	stateDir := filepath.Dir(a.Cfg.Gateway.StatePath)
	ConfigMu.Unlock()
	// The running pieces follow the file.
	for id, h := range harnesses {
		if err := a.Manager.Set(id, harness.Config{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: runtime.ApplyEnv(h.Env, id, stateDir), Permission: h.Permission}); err != nil {
			slog.Error(fmt.Sprintf("steve: harness %s: %v", id, err), "harness", id)
		}
	}
	for id := range old.Harnesses {
		if _, keep := harnesses[id]; !keep {
			a.Manager.Remove(id)
		}
	}
	caps := make(map[string]capability.MCPServer, len(servers))
	for id, m := range servers {
		caps[id] = capability.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	a.Assembler.SetServers(caps)
	a.Fleet.SetHubCapabilities(set.Capabilities)
	ConfigMu.RLock()
	slots := a.Cfg.HubSlots()
	ConfigMu.RUnlock()
	a.Fleet.SetHubSlots(slots)
	if a.Observation != nil {
		a.Observation.Launch.Wake()
	}
	slog.Info(fmt.Sprintf("steve: hub settings applied from the page: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(harnesses), len(set.Tools), len(servers), len(set.Declares), len(set.Capabilities)))
	return a.hubSettings(), saveErr
}

func (a *Service) hubHarnessSettings(settings map[string]nodewire.HarnessSetting) (map[string]config.Harness, error) {
	harnesses := make(map[string]config.Harness, len(settings))
	for id, h := range settings {
		if !NameShape.MatchString(strings.ToLower(id)) || strings.TrimSpace(h.Command) == "" {
			return nil, fmt.Errorf("AI 工具 %q 需要一个合法的名字和启动命令", id)
		}
		ConfigMu.RLock()
		item := a.Cfg.Harnesses[id]
		ConfigMu.RUnlock()
		if h.Adapter != nil && *h.Adapter != item.Adapter {
			return nil, fmt.Errorf("更换 %s 的 adapter 需要通过配置文件重启生效", id)
		}
		if item.Adapter != "" && h.Command != item.Command {
			return nil, fmt.Errorf("%s 使用固定 adapter，不能直接更换生成的启动命令", id)
		}
		item.Command, item.Args, item.ProcessDir = h.Command, h.Args, h.ProcessDir
		if h.Env != nil {
			item.Env = h.Env
		}
		if h.Slots != nil {
			if *h.Slots < 0 {
				return nil, fmt.Errorf("%s slots must be nonnegative", id)
			}
			item.Slots = *h.Slots
		}
		if h.Permission != nil {
			item.Permission = *h.Permission
		}
		if item.Permission == "" {
			item.Permission = config.PermissionRead
		}
		if _, err := permission.New(item.Permission); err != nil {
			return nil, err
		}
		harnesses[id] = item
	}
	return harnesses, nil
}

func (a *Service) hubMCPSettings(settings map[string]nodewire.MCPSetting) (map[string]config.MCPServer, error) {

	servers := make(map[string]config.MCPServer, len(settings))
	for id, m := range settings {
		ConfigMu.RLock()
		old := a.Cfg.MCPServers[id]
		ConfigMu.RUnlock()
		if m.Env == nil {
			m.Env = old.Env
		}
		if m.Headers == nil {
			m.Headers = old.Headers
		}
		if !NameShape.MatchString(strings.ToLower(id)) {
			return nil, fmt.Errorf("MCP 服务器 %q 的名字不合法", id)
		}
		switch m.Type {
		case "", "stdio":
			if strings.TrimSpace(m.Command) == "" {
				return nil, fmt.Errorf("MCP 服务器 %q 需要启动命令", id)
			}
			m.Type = "stdio"
		case "http", "sse":
			if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
				return nil, fmt.Errorf("MCP 服务器 %q 需要 http(s) 地址", id)
			}
		default:
			return nil, fmt.Errorf("MCP 服务器 %q：不认识的类型 %q", id, m.Type)
		}
		servers[id] = config.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	return servers, nil
}

func (a *Service) hubSettings() nodewire.Settings {
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, a.Cfg.Gateway.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, a.Cfg.Gateway.Declares...), Capabilities: append([]string{}, a.Cfg.Gateway.Capabilities...)}
	for id, h := range a.Cfg.Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Adapter: &h.Adapter, Slots: &h.Slots, Permission: &h.Permission, Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir}
	}
	for id, m := range a.Cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	out.Revision = nodewire.SettingsRevision(out)
	return nodewire.CloneSettings(out)
}

func (a *Service) AddNode(ctx context.Context, req consoleapi.AddNodeRequest) (consoleapi.AddNodeResult, error) {
	name := strings.TrimSpace(req.Name)
	if !NameShape.MatchString(name) {
		return consoleapi.AddNodeResult{}, fmt.Errorf("机器名只能是小写字母、数字、点、下划线、连字符，如 node-c")
	}
	addr := strings.TrimSpace(req.Addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return consoleapi.AddNodeResult{}, fmt.Errorf("地址要写成 ip:端口，如 10.0.0.5:7701")
	}
	level := project.Level(strings.TrimSpace(req.Level)).OrDefault()
	if _, ok := map[project.Level]bool{project.LevelPublic: true, project.LevelInternal: true, project.LevelRestricted: true, project.LevelSealed: true}[level]; !ok {
		return consoleapi.AddNodeResult{}, fmt.Errorf("数据等级只能是 public / internal / restricted / sealed")
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return consoleapi.AddNodeResult{}, err
	}
	token := hex.EncodeToString(raw[:])
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ConfigMu.Lock()
	if _, exists := a.Cfg.Nodes[name]; exists {
		ConfigMu.Unlock()
		return consoleapi.AddNodeResult{}, fmt.Errorf("机器 %s 已经存在", name)
	}
	if name == NodeName() {
		ConfigMu.Unlock()
		return consoleapi.AddNodeResult{}, fmt.Errorf("%s 是 hub 自己", name)
	}
	if a.Cfg.Nodes == nil {
		a.Cfg.Nodes = map[string]config.Node{}
	}
	a.Cfg.Nodes[name] = config.Node{Addr: addr, Token: token, Level: string(level)}
	saveErr := a.persistConfigContext(ctx, a.Cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		delete(a.Cfg.Nodes, name)
		ConfigMu.Unlock()
		return consoleapi.AddNodeResult{}, saveErr
	}
	levels, regions := a.Cfg.NodeLevels(), a.Cfg.NodeRegions()
	binary := a.Cfg.Gateway.NodeBinary
	ConfigMu.Unlock()
	a.hubURL = req.HubURL
	a.Nodes.Add(name, node.Config{Addr: addr, Token: token, Level: string(level)})
	a.Fleet.SetNodeLevels(levels)
	a.Fleet.SetNodeRegions(regions)
	slog.Info(fmt.Sprintf("steve: machine %s added (%s, %s); waiting for it to come up", name, addr, level), "node", name)
	out := consoleapi.AddNodeResult{Name: name, Token: token,
		Command: fmt.Sprintf("curl -fsSL '%s/bootstrap/%s?token=%s' | bash -l", req.HubURL, name, token)}
	if binary == "" {
		out.Note = "协调节点尚未配置手动安装包。请先将 steve-node 放到目标机器的 ~/steve-bin/steve-node。如需改用 SSH 自动安装，请先移除此未接入的机器登记，再从“通过 SSH 接入”重新添加。"
	}
	return out, saveErr
}

// RemoveNode forgets a machine. Nothing may still live on it: an agent
// placed there, a project homed there or with a copy there, keep it.
func (a *Service) RemoveNode(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == NodeName() || name == "hub" {
		return fmt.Errorf("%q 是 hub 自己，不能移除", name)
	}
	// Placement checks and removal share the same administration boundary as
	// agent/project updates; no new owner can arrive between check and delete.
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.Catalog != nil {
		for _, ag := range a.Catalog.List() {
			if ag.Node == name {
				return fmt.Errorf("Agent %s 还在 %s 上；先把它移到别的机器或删掉", ag.ID, name)
			}
		}
	}
	if a.Projects != nil {
		list, err := a.Projects.List(ctx)
		if err != nil {
			return fmt.Errorf("检查机器 %s 的项目占用失败：%w", name, err)
		}
		for _, p := range list {
			for _, ws := range p.Workspaces() {
				if ws.Node == name {
					kind := "主目录"
					if ws.Kind == project.KindCopy {
						kind = "副本"
					}
					return fmt.Errorf("项目 %s 的%s还在 %s 上；先移除它", p.ID, kind, name)
				}
			}
		}
	}
	ConfigMu.Lock()
	saved, inConfig := a.Cfg.Nodes[name]
	if !inConfig {
		ConfigMu.Unlock()
		return fmt.Errorf("没有叫 %q 的机器", name)
	}
	delete(a.Cfg.Nodes, name)
	saveErr := a.persistConfigContext(ctx, a.Cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		a.Cfg.Nodes[name] = saved
		ConfigMu.Unlock()
		return saveErr
	}
	levels, regions := a.Cfg.NodeLevels(), a.Cfg.NodeRegions()
	ConfigMu.Unlock()
	a.Nodes.Remove(name)
	a.Fleet.SetNodeLevels(levels)
	a.Fleet.SetNodeRegions(regions)
	slog.Info(fmt.Sprintf("steve: machine %s removed", name), "node", name)
	return saveErr
}

// Bootstrap is the script a new machine runs: it writes node.json with
// the hub's harness commands, fetches steve-node from the hub when the
// hub has one, and starts it from a login shell so the harnesses find
// their credentials.
func (a *Service) Bootstrap(name, token string) (string, bool) {
	a.Mu.Lock()
	ConfigMu.RLock()
	n, ok := a.Cfg.Nodes[name]
	harnesses := make(map[string]nodebootstrap.Harness, len(a.Cfg.Harnesses))
	for id, h := range a.Cfg.Harnesses {
		if h.Adapter != "" {
			harnesses[id] = nodebootstrap.Harness{Adapter: h.Adapter}
		} else {
			harnesses[id] = nodebootstrap.Harness{Command: h.Command, Args: append([]string(nil), h.Args...)}
		}
	}
	binary, hubURL := a.Cfg.Gateway.NodeBinary, a.hubURL
	ConfigMu.RUnlock()
	a.Mu.Unlock()
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) != 1 {
		return "", false
	}
	_, port, _ := net.SplitHostPort(n.Addr)
	if port == "" {
		port = "7701"
	}
	spec := nodebootstrap.Spec{Name: name, Port: port, Token: token, Harnesses: harnesses}
	if binary != "" && hubURL != "" {
		metadata, err := nodebootstrap.InspectBinary(binary)
		if err != nil {
			return "", false
		}
		spec.DownloadURL = strings.TrimRight(hubURL, "/") + "/dist/steve-node?" + url.Values{"token": {token}}.Encode()
		spec.OS, spec.Arch, spec.SHA256 = metadata.OS, metadata.Arch, metadata.SHA256
	}
	script, err := nodebootstrap.Build(spec)
	return script, err == nil
}

func (a *Service) NodeBinary(token string) (string, bool) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	if a.Cfg.Gateway.NodeBinary == "" || token == "" {
		return "", false
	}
	for _, n := range a.Cfg.Nodes {
		if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) == 1 {
			return a.Cfg.Gateway.NodeBinary, true
		}
	}
	return "", false
}
