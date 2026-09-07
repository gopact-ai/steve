package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
func (a *fleetAdmin) NodeSettings(ctx context.Context, name string) (nodewire.Settings, error) {
	if name == nodeName() && !a.clusterMode {
		return a.hubSettings(), nil
	}
	return a.nodes.Settings(ctx, name)
}

// SetNodeSettings rewrites what a machine offers and answers what is in
// force: on the hub, the config file and the running manager, assembler,
// roster and launch probe; on a node, the node itself.
func (a *fleetAdmin) SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if name != nodeName() || a.clusterMode {
		return a.nodes.Configure(ctx, name, set)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setNodeSettingsLocked(ctx, name, set)
}

// setNodeSettingsLocked is SetNodeSettings for the hub with a.mu held.
func (a *fleetAdmin) setNodeSettingsLocked(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	_ = ctx
	if set.Revision == "" || set.Revision != a.hubSettings().Revision {
		return nodewire.Settings{}, nodewire.ErrSettingsRevisionConflict
	}
	set = nodewire.CloneSettings(set)
	harnesses := make(map[string]config.Harness, len(set.Harnesses))
	for id, h := range set.Harnesses {
		if !nameShape.MatchString(strings.ToLower(id)) || strings.TrimSpace(h.Command) == "" {
			return nodewire.Settings{}, fmt.Errorf("AI 工具 %q 需要一个合法的名字和启动命令", id)
		}
		configMu.RLock()
		item := a.cfg.Harnesses[id]
		configMu.RUnlock()
		if h.Adapter != nil && *h.Adapter != item.Adapter {
			return nodewire.Settings{}, fmt.Errorf("更换 %s 的 adapter 需要通过配置文件重启生效", id)
		}
		if item.Adapter != "" && h.Command != item.Command {
			return nodewire.Settings{}, fmt.Errorf("%s 使用固定 adapter，不能直接更换生成的启动命令", id)
		}
		item.Command, item.Args, item.ProcessDir = h.Command, h.Args, h.ProcessDir
		if h.Env != nil {
			item.Env = h.Env
		}
		if h.Slots != nil {
			if *h.Slots < 0 {
				return nodewire.Settings{}, fmt.Errorf("%s slots must be nonnegative", id)
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
			return nodewire.Settings{}, err
		}
		harnesses[id] = item
	}
	servers := make(map[string]config.MCPServer, len(set.MCPServers))
	for id, m := range set.MCPServers {
		configMu.RLock()
		old := a.cfg.MCPServers[id]
		configMu.RUnlock()
		if m.Env == nil {
			m.Env = old.Env
		}
		if m.Headers == nil {
			m.Headers = old.Headers
		}
		if !nameShape.MatchString(strings.ToLower(id)) {
			return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 的名字不合法", id)
		}
		switch m.Type {
		case "", "stdio":
			if strings.TrimSpace(m.Command) == "" {
				return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 需要启动命令", id)
			}
			m.Type = "stdio"
		case "http", "sse":
			if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
				return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 需要 http(s) 地址", id)
			}
		default:
			return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q：不认识的类型 %q", id, m.Type)
		}
		servers[id] = config.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	for _, d := range set.Declares {
		if !strings.Contains(d, ":") {
			return nodewire.Settings{}, fmt.Errorf("声明 %q 要写成 kind:id，如 network:office", d)
		}
	}
	configMu.Lock()
	// Agents keep naming harnesses that exist.
	for id, ag := range a.cfg.Agents {
		if ag.Node == "" {
			if _, ok := harnesses[ag.Harness]; !ok {
				configMu.Unlock()
				return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 AI 工具 %s，不能删", id, ag.Harness)
			}
			for _, srv := range ag.MCPServers {
				if _, ok := servers[srv]; !ok {
					configMu.Unlock()
					return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 MCP 服务器 %s，不能删", id, srv)
				}
			}
		}
	}
	old := *a.cfg
	a.cfg.Harnesses = harnesses
	a.cfg.MCPServers = servers
	a.cfg.Gateway.Tools = append([]string(nil), set.Tools...)
	a.cfg.Gateway.Declares = append([]string(nil), set.Declares...)
	a.cfg.Gateway.Capabilities = append([]string(nil), set.Capabilities...)
	saveErr := a.persistConfig(a.cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		a.cfg.Harnesses, a.cfg.MCPServers, a.cfg.Gateway = old.Harnesses, old.MCPServers, old.Gateway
		configMu.Unlock()
		if errors.Is(saveErr, config.ErrFileChanged) {
			return nodewire.Settings{}, errors.Join(nodewire.ErrSettingsRevisionConflict, saveErr)
		}
		return nodewire.Settings{}, saveErr
	}
	stateDir := filepath.Dir(a.cfg.Gateway.StatePath)
	configMu.Unlock()
	// The running pieces follow the file.
	for id, h := range harnesses {
		if err := a.manager.Set(id, harness.Config{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: runtime.ApplyEnv(h.Env, id, stateDir), Permission: h.Permission}); err != nil {
			log.Printf("steve: harness %s: %v", id, err)
		}
	}
	for id := range old.Harnesses {
		if _, keep := harnesses[id]; !keep {
			a.manager.Remove(id)
		}
	}
	caps := make(map[string]capability.MCPServer, len(servers))
	for id, m := range servers {
		caps[id] = capability.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	a.assembler.SetServers(caps)
	a.fleet.SetHubCapabilities(set.Capabilities)
	configMu.RLock()
	slots := a.cfg.HubSlots()
	configMu.RUnlock()
	a.fleet.SetHubSlots(slots)
	if a.observation != nil {
		a.observation.launch.Wake()
	}
	log.Printf("steve: hub settings applied from the page: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(harnesses), len(set.Tools), len(servers), len(set.Declares), len(set.Capabilities))
	return a.hubSettings(), saveErr
}

func (a *fleetAdmin) hubSettings() nodewire.Settings {
	configMu.RLock()
	defer configMu.RUnlock()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, a.cfg.Gateway.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, a.cfg.Gateway.Declares...), Capabilities: append([]string{}, a.cfg.Gateway.Capabilities...)}
	for id, h := range a.cfg.Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Adapter: &h.Adapter, Slots: &h.Slots, Permission: &h.Permission, Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir}
	}
	for id, m := range a.cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	out.Revision = nodewire.SettingsRevision(out)
	return nodewire.CloneSettings(out)
}

func (a *fleetAdmin) AddNode(ctx context.Context, req consoleapi.AddNodeRequest) (consoleapi.AddNodeResult, error) {
	name := strings.TrimSpace(req.Name)
	if !nameShape.MatchString(name) {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	if _, exists := a.cfg.Nodes[name]; exists {
		configMu.Unlock()
		return consoleapi.AddNodeResult{}, fmt.Errorf("机器 %s 已经存在", name)
	}
	if name == nodeName() {
		configMu.Unlock()
		return consoleapi.AddNodeResult{}, fmt.Errorf("%s 是 hub 自己", name)
	}
	if a.cfg.Nodes == nil {
		a.cfg.Nodes = map[string]config.Node{}
	}
	a.cfg.Nodes[name] = config.Node{Addr: addr, Token: token, Level: string(level)}
	saveErr := a.persistConfigContext(ctx, a.cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		delete(a.cfg.Nodes, name)
		configMu.Unlock()
		return consoleapi.AddNodeResult{}, saveErr
	}
	levels, regions := a.cfg.NodeLevels(), a.cfg.NodeRegions()
	binary := a.cfg.Gateway.NodeBinary
	configMu.Unlock()
	a.hubURL = req.HubURL
	a.nodes.Add(name, node.Config{Addr: addr, Token: token, Level: string(level)})
	a.fleet.SetNodeLevels(levels)
	a.fleet.SetNodeRegions(regions)
	log.Printf("steve: machine %s added (%s, %s); waiting for it to come up", name, addr, level)
	out := consoleapi.AddNodeResult{Name: name, Token: token,
		Command: fmt.Sprintf("curl -fsSL '%s/bootstrap/%s?token=%s' | bash -l", req.HubURL, name, token)}
	if binary == "" {
		out.Note = "hub 没有配置 gateway.node_binary，脚本不会下载 steve-node：先把它放到那台机器的 ~/steve-bin/steve-node。"
	}
	return out, saveErr
}

// RemoveNode forgets a machine. Nothing may still live on it: an agent
// placed there, a project homed there or with a copy there, keep it.
func (a *fleetAdmin) RemoveNode(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == nodeName() || name == "hub" {
		return fmt.Errorf("%q 是 hub 自己，不能移除", name)
	}
	// Placement checks and removal share the same administration boundary as
	// agent/project updates; no new owner can arrive between check and delete.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.catalog != nil {
		for _, ag := range a.catalog.List() {
			if ag.Node == name {
				return fmt.Errorf("Agent %s 还在 %s 上；先把它移到别的机器或删掉", ag.ID, name)
			}
		}
	}
	if a.projects != nil {
		list, err := a.projects.List(ctx)
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
	configMu.Lock()
	saved, inConfig := a.cfg.Nodes[name]
	if !inConfig {
		configMu.Unlock()
		return fmt.Errorf("没有叫 %q 的机器", name)
	}
	delete(a.cfg.Nodes, name)
	saveErr := a.persistConfigContext(ctx, a.cfg)
	if saveErr != nil && !config.Committed(saveErr) {
		a.cfg.Nodes[name] = saved
		configMu.Unlock()
		return saveErr
	}
	levels, regions := a.cfg.NodeLevels(), a.cfg.NodeRegions()
	configMu.Unlock()
	a.nodes.Remove(name)
	a.fleet.SetNodeLevels(levels)
	a.fleet.SetNodeRegions(regions)
	log.Printf("steve: machine %s removed", name)
	return saveErr
}

// Bootstrap is the script a new machine runs: it writes node.json with
// the hub's harness commands, fetches steve-node from the hub when the
// hub has one, and starts it from a login shell so the harnesses find
// their credentials.
func (a *fleetAdmin) Bootstrap(name, token string) (string, bool) {
	a.mu.Lock()
	configMu.RLock()
	n, ok := a.cfg.Nodes[name]
	harnesses := make(map[string]nodebootstrap.Harness, len(a.cfg.Harnesses))
	for id, h := range a.cfg.Harnesses {
		if h.Adapter != "" {
			harnesses[id] = nodebootstrap.Harness{Adapter: h.Adapter}
		} else {
			harnesses[id] = nodebootstrap.Harness{Command: h.Command, Args: append([]string(nil), h.Args...)}
		}
	}
	binary, hubURL := a.cfg.Gateway.NodeBinary, a.hubURL
	configMu.RUnlock()
	a.mu.Unlock()
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

func (a *fleetAdmin) NodeBinary(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.RLock()
	defer configMu.RUnlock()
	if a.cfg.Gateway.NodeBinary == "" || token == "" {
		return "", false
	}
	for _, n := range a.cfg.Nodes {
		if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) == 1 {
			return a.cfg.Gateway.NodeBinary, true
		}
	}
	return "", false
}
