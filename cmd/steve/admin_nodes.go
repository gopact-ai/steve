package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
)

// NodeSettings reads what a machine offers: the hub's own from its
// config, a node's from the node.
func (a *fleetAdmin) NodeSettings(ctx context.Context, name string) (nodewire.Settings, error) {
	if name == nodeName() {
		return a.hubSettings(), nil
	}
	return a.nodes.Settings(ctx, name)
}

// SetNodeSettings rewrites what a machine offers and answers what is in
// force: on the hub, the config file and the running manager, assembler,
// roster and launch probe; on a node, the node itself.
func (a *fleetAdmin) SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if name != nodeName() {
		return a.nodes.Configure(ctx, name, set)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setNodeSettingsLocked(ctx, name, set)
}

// setNodeSettingsLocked is SetNodeSettings for the hub with a.mu held.
func (a *fleetAdmin) setNodeSettingsLocked(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	_ = ctx
	if len(set.Harnesses) == 0 {
		return nodewire.Settings{}, fmt.Errorf("hub 至少要有一个 AI 工具")
	}
	harnesses := make(map[string]config.Harness, len(set.Harnesses))
	for id, h := range set.Harnesses {
		if !nameShape.MatchString(strings.ToLower(id)) || strings.TrimSpace(h.Command) == "" {
			return nodewire.Settings{}, fmt.Errorf("AI 工具 %q 需要一个合法的名字和启动命令", id)
		}
		item := config.Harness{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: h.Env}
		configMu.RLock()
		if old, ok := a.cfg.Harnesses[id]; ok {
			item.Permission = old.Permission
		}
		configMu.RUnlock()
		harnesses[id] = item
	}
	servers := make(map[string]config.MCPServer, len(set.MCPServers))
	for id, m := range set.MCPServers {
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
		return nodewire.Settings{}, saveErr
	}
	configMu.Unlock()
	// The running pieces follow the file.
	for id, h := range harnesses {
		if err := a.manager.Set(id, harness.Config{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: h.Env, Permission: h.Permission}); err != nil {
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
	hubLaunch.Wake()
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
		out.Harnesses[id] = nodewire.HarnessSetting{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir}
	}
	for id, m := range a.cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	return out
}

func (a *fleetAdmin) AddNode(_ context.Context, req consoleapi.AddNodeRequest) (consoleapi.AddNodeResult, error) {
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
	saveErr := a.persistConfig(a.cfg)
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
	saveErr := a.persistConfig(a.cfg)
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
	harnesses := make(map[string]any, len(a.cfg.Harnesses))
	for id, h := range a.cfg.Harnesses {
		spec := map[string]any{"command": h.Command}
		if len(h.Args) > 0 {
			spec["args"] = h.Args
		}
		harnesses[id] = spec
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
	nodeJSON, _ := json.MarshalIndent(map[string]any{
		"name": name, "listen": "0.0.0.0:" + port, "token": token,
		"workspace_root": "~/steve-work", "state_dir": "~/.steve-node", "harnesses": harnesses,
	}, "", "  ")
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -e\nmkdir -p ~/steve-bin ~/steve-work\n")
	fmt.Fprintf(&b, "cat > ~/steve-bin/node.json <<'STEVE_EOF'\n%s\nSTEVE_EOF\nchmod 600 ~/steve-bin/node.json\n", nodeJSON)
	if binary != "" && hubURL != "" {
		fmt.Fprintf(&b, "if [ ! -x ~/steve-bin/steve-node ]; then curl -fsSL '%s/dist/steve-node?token=%s' -o ~/steve-bin/steve-node && chmod +x ~/steve-bin/steve-node; fi\n", hubURL, token)
	}
	b.WriteString("if [ ! -x ~/steve-bin/steve-node ]; then echo 'steve-node is not in ~/steve-bin; copy it there and run this again' >&2; exit 1; fi\n")
	b.WriteString("for p in $(pgrep -f 'steve-bin/steve-node -config' 2>/dev/null); do kill \"$p\" 2>/dev/null || true; done\n")
	b.WriteString("nohup bash -lc \"~/steve-bin/steve-node -config ~/steve-bin/node.json\" > ~/steve-node.log 2>&1 < /dev/null &\n")
	fmt.Fprintf(&b, "echo 'steve-node %s started; log: ~/steve-node.log'\n", name)
	return b.String(), true
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
