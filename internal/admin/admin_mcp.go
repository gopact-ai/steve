package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/mcpprobe"
	"github.com/gopact-ai/steve/internal/mcpregistry"
	"github.com/gopact-ai/steve/internal/mcpscan"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type mcpProbeEntry struct {
	view  consoleapi.MCPProbeView
	shape string
}

// mcpShape digests the public shape of a deployment: transport, command,
// arguments, URL, and the names of its variables and headers. A probe
// taken under one shape is stale under another.
func mcpShape(s nodewire.MCPSetting) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", s.Type, s.Command, strings.Join(s.Args, "\x01"), s.URL)
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(h, "e:%s\x00", k)
	}
	for _, k := range sortedKeys(s.Headers) {
		fmt.Fprintf(h, "h:%s\x00", k)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reservedMCP are names the platform gives its own session servers.
func reservedMCP(name string) bool { return name == "feishu" || strings.HasPrefix(name, "steve") }

// mcpSettingsOf reads a machine's MCP settings: the hub's from config,
// a node's from the node (values included, as the config stream returns
// them; the page is handed only the keys).
func (a *Service) mcpSettingsOf(ctx context.Context, nodeKey string) (map[string]nodewire.MCPSetting, error) {
	if nodeKey == "" {
		return a.hubSettings().MCPServers, nil
	}
	set, err := a.Nodes.Settings(ctx, nodeKey)
	if err != nil {
		return nil, err
	}
	return set.MCPServers, nil
}

// advertOf is what a machine last said about itself; the hub's is made now.
func (a *Service) advertOf(ctx context.Context, nodeKey string) (nodewire.Advert, error) {
	if nodeKey == "" {
		return ObservedHubAdvert(a.Cfg, a.Observation), nil
	}
	return a.Nodes.Advert(ctx, nodeKey)
}

// MCP is the MCP page: every deployment on every machine with who
// attaches and the last probe, the platform's session servers, and
// what each machine's coding agents configured themselves.
func (a *Service) MCP(ctx context.Context) (consoleapi.MCPView, error) {
	view := consoleapi.MCPView{Deployments: []consoleapi.MCPDeployment{}, Platform: []consoleapi.MCPPlatform{}, Machines: []consoleapi.MCPMachine{}}
	attach := map[string][]string{}
	if a.Catalog != nil {
		for _, ag := range a.Catalog.List() {
			for _, m := range ag.MCPServers {
				key := nodewire.Place(ag.Node) + "/" + m
				attach[key] = append(attach[key], ag.ID)
			}
		}
	}
	counts := map[string]int{}
	a.probeMu.Lock()
	probes := make(map[string]mcpProbeEntry, len(a.probes))
	for k, v := range a.probes {
		probes[k] = v
	}
	provenance := make(map[string]string, len(a.provenance))
	for k, v := range a.provenance {
		provenance[k] = v
	}
	a.probeMu.Unlock()
	for _, nodeKey := range append([]string{""}, a.Nodes.Names()...) {
		place := nodewire.Place(nodeKey)
		adv, advErr := a.advertOf(ctx, nodeKey)
		resolvable := map[string]*bool{}
		if adv.Snapshot != nil {
			for _, c := range adv.Snapshot.Offers {
				if c.Kind != ability.MCP {
					continue
				}
				for _, e := range c.Evidence {
					ok := e.OK
					resolvable[c.ID] = &ok
				}
			}
		}
		settings, err := a.mcpSettingsOf(ctx, nodeKey)
		if err != nil && adv.Snapshot != nil {
			// The machine cannot be asked right now: what it last said
			// it had is still worth listing.
			settings = map[string]nodewire.MCPSetting{}
			for _, c := range adv.Snapshot.Offers {
				if c.Kind == ability.MCP {
					settings[c.ID] = nodewire.MCPSetting{Type: c.Attrs["transport"]}
				}
			}
		}
		for _, name := range sortedMCP(settings) {
			set := settings[name]
			d := consoleapi.MCPDeployment{Node: place, Name: name, Type: set.Type, Command: set.Command, Args: set.Args, URL: set.URL, EnvKeys: sortedKeys(set.Env), HeaderKeys: sortedKeys(set.Headers), Agents: attach[place+"/"+name], Resolvable: resolvable[name], Provenance: provenance[place+"/"+name]}
			if d.Agents == nil {
				d.Agents = []string{}
			}
			if p, ok := probes[place+"/"+name]; ok {
				pv := p.view
				if p.shape != "" && p.shape != mcpShape(set) {
					pv.Stale = true
				}
				d.Probe = &pv
			}
			counts[name]++
			view.Deployments = append(view.Deployments, d)
		}
		m := consoleapi.MCPMachine{Name: place, Hub: nodeKey == "", Up: advErr == nil, Own: []consoleapi.MCPOwn{}}
		own := adv.OwnMCP
		if nodeKey == "" {
			own = node.OwnMCP(5 * time.Minute)
		} else if advErr == nil && !nodewire.HasFeature(adv.Features, nodewire.FeatureMCPProbe) {
			m.Unsupported = true
		}
		for _, o := range own {
			_, adopted := settings[o.Name]
			m.Own = append(m.Own, consoleapi.MCPOwn{Name: o.Name, Source: o.Source, Scope: o.Scope, Type: o.Type, Command: o.Command, Args: o.Args, URL: o.URL, EnvKeys: o.EnvKeys, HeaderKeys: o.HeaderKeys, Adopted: adopted})
		}
		view.Machines = append(view.Machines, m)
	}
	for i := range view.Deployments {
		view.Deployments[i].SameNameElsewhere = counts[view.Deployments[i].Name] > 1
	}
	tools := []consoleapi.PlatformTool{}
	for _, t := range agentmcp.PlatformTools(true) {
		tools = append(tools, consoleapi.PlatformTool{Name: t.Name, Description: t.Description})
	}
	view.Platform = append(view.Platform, consoleapi.MCPPlatform{Name: agentmcp.ServerName, Description: "hub 为每个会话现场生成：你在哪（steve_context）、项目在哪（steve_projects）、做法（steve_help）、进度卡，以及（接了委派时）看机器、委派、等结果", Tools: tools})
	return view, nil
}

func sortedMCP(m map[string]nodewire.MCPSetting) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProbeMCP asks a deployment what tools it offers — on the hub directly,
// on a node through the node, which binds it the way a session would —
// and remembers the answer. A failed probe keeps the last good tool
// list, marked stale, beside the error.
func (a *Service) ProbeMCP(ctx context.Context, machine, name string) (consoleapi.MCPProbeView, error) {
	nodeKey := a.nodeKey(machine)
	place := nodewire.Place(nodeKey)
	settings, err := a.mcpSettingsOf(ctx, nodeKey)
	if err != nil {
		return consoleapi.MCPProbeView{}, err
	}
	set, ok := settings[name]
	if !ok {
		return consoleapi.MCPProbeView{}, fmt.Errorf("%s 上没有叫 %q 的 MCP 服务器", place, name)
	}
	view := consoleapi.MCPProbeView{At: time.Now().UTC(), Tools: []nodewire.MCPTool{}}
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var reply nodewire.MCPProbeReply
	if nodeKey == "" {
		result, perr := mcpprobe.Probe(pctx, mcpprobe.Server{Type: set.Type, Command: set.Command, Args: set.Args, Env: set.Env, URL: set.URL, Headers: set.Headers})
		if perr != nil {
			reply.Error = perr.Error()
		} else {
			reply = nodewire.MCPProbeReply{ServerName: result.ServerName, ServerVersion: result.ServerVersion, Protocol: result.Protocol, Digest: result.Digest, ElapsedMS: result.Elapsed.Milliseconds()}
			for _, t := range result.Tools {
				reply.Tools = append(reply.Tools, nodewire.MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
			}
		}
	} else {
		reply, err = a.Nodes.MCPProbe(pctx, nodeKey, name)
		if err != nil {
			reply.Error = err.Error()
		}
	}
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.probes == nil {
		a.probes = map[string]mcpProbeEntry{}
	}
	if reply.Error != "" {
		view.Error = Clip(reply.Error, 600)
		if prev, had := a.probes[place+"/"+name]; had && prev.view.OK {
			view.Tools, view.Digest, view.Stale = prev.view.Tools, prev.view.Digest, true
			view.ServerName, view.ServerVersion = prev.view.ServerName, prev.view.ServerVersion
		}
	} else {
		view.OK = true
		view.Tools, view.Digest, view.ServerName, view.ServerVersion, view.Protocol = reply.Tools, reply.Digest, reply.ServerName, reply.ServerVersion, reply.Protocol
		if view.Tools == nil {
			view.Tools = []nodewire.MCPTool{}
		}
	}
	a.probes[place+"/"+name] = mcpProbeEntry{view: view, shape: mcpShape(set)}
	return view, nil
}

func (a *Service) remember(place, name, where string) {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.provenance == nil {
		a.provenance = map[string]string{}
	}
	if where == "" {
		delete(a.provenance, place+"/"+name)
		delete(a.probes, place+"/"+name)
		return
	}
	a.provenance[place+"/"+name] = where
}

// AdoptMCP copies a coding agent's own MCP server into a machine's
// settings, on that machine: the hub's from its own files, a node's by
// telling the node to. Values never pass through here.
func (a *Service) AdoptMCP(ctx context.Context, machine, source, name string) error {
	nodeKey := a.nodeKey(machine)
	if reservedMCP(name) {
		return fmt.Errorf("%q 是平台自己用的名字，换个名字再纳入", name)
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if nodeKey != "" {
		if _, err := a.Nodes.AdoptMCP(ctx, nodeKey, source, name); err != nil {
			return err
		}
		a.remember(nodewire.Place(nodeKey), name, "adopted:"+source)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	full, ok := mcpscan.Lookup(home, source, name)
	if !ok {
		return fmt.Errorf("hub 这个用户的 %s 配置里没有 %q", source, name)
	}
	set := a.hubSettings()
	if _, exists := set.MCPServers[name]; exists {
		return fmt.Errorf("hub 上已经有叫 %q 的 MCP 服务器；先删掉它，或换个名字", name)
	}
	if set.MCPServers == nil {
		set.MCPServers = map[string]nodewire.MCPSetting{}
	}
	set.MCPServers[name] = nodewire.MCPSetting{Type: full.Type, Command: full.Command, Args: full.Args, Env: full.Env, URL: full.URL, Headers: full.Headers}
	if _, err := a.setNodeSettingsLocked(ctx, NodeName(), set); err != nil {
		return err
	}
	a.remember(nodewire.Place(""), name, "adopted:"+source)
	node.OwnMCP(0)
	return nil
}

// RemoveMCP drops a deployment from a machine's settings. An agent on
// that machine still naming it keeps it.
func (a *Service) RemoveMCP(ctx context.Context, machine, name string) error {
	nodeKey := a.nodeKey(machine)
	place := nodewire.Place(nodeKey)
	if a.Catalog != nil {
		for _, ag := range a.Catalog.List() {
			if nodewire.Place(ag.Node) == place && slices.Contains(ag.MCPServers, name) {
				return fmt.Errorf("Agent %s 还在用 %s 上的 %q；先在资源页把它从 Agent 的 MCP 列表里去掉", ag.ID, place, name)
			}
		}
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	var set nodewire.Settings
	var err error
	if nodeKey == "" {
		set = a.hubSettings()
	} else if set, err = a.Nodes.Settings(ctx, nodeKey); err != nil {
		return err
	}
	if _, ok := set.MCPServers[name]; !ok {
		return fmt.Errorf("%s 上没有叫 %q 的 MCP 服务器", place, name)
	}
	delete(set.MCPServers, name)
	if nodeKey == "" {
		_, err = a.setNodeSettingsLocked(ctx, NodeName(), set)
	} else {
		_, err = a.Nodes.Configure(ctx, nodeKey, set)
	}
	if err != nil {
		return err
	}
	a.remember(place, name, "")
	return nil
}

// SearchMCPRegistry asks the official registry.
func (a *Service) SearchMCPRegistry(ctx context.Context, q string) ([]consoleapi.MCPRegistryEntry, error) {
	entries, err := mcpregistry.Search(ctx, q, 20)
	if err != nil {
		return nil, err
	}
	out := make([]consoleapi.MCPRegistryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, registryEntryView(e))
	}
	return out, nil
}

func registryEntryView(e mcpregistry.Entry) consoleapi.MCPRegistryEntry {
	out := consoleapi.MCPRegistryEntry{Name: e.Name, Description: e.Description, Version: e.Version, Repository: e.Repository, Packages: []consoleapi.MCPRegistryPackage{}, Remotes: []consoleapi.MCPRegistryRemote{}}
	envs := func(in []mcpregistry.EnvVar) []consoleapi.MCPRegistryEnv {
		vs := make([]consoleapi.MCPRegistryEnv, 0, len(in))
		for _, v := range in {
			vs = append(vs, consoleapi.MCPRegistryEnv{Name: v.Name, Description: v.Description, Required: v.Required, Secret: v.Secret, Default: v.Default})
		}
		return vs
	}
	for _, p := range e.Packages {
		out.Packages = append(out.Packages, consoleapi.MCPRegistryPackage{RegistryType: p.RegistryType, Identifier: p.Identifier, Version: p.Version, RuntimeHint: p.RuntimeHint, Transport: p.Transport, Needs: p.Needs, Env: envs(p.Env)})
	}
	for _, r := range e.Remotes {
		out.Remotes = append(out.Remotes, consoleapi.MCPRegistryRemote{Type: r.Type, URL: r.URL, Headers: envs(r.Headers)})
	}
	return out
}

// InstallMCP puts a registry entry on a machine: the chosen package
// becomes a command the machine must be able to run, the chosen remote
// an address that may not point inside; what the entry asked for is
// filled from what the owner typed and sent to that machine once.
func (a *Service) InstallMCP(ctx context.Context, req consoleapi.InstallMCPRequest) error {
	nodeKey := a.nodeKey(req.Node)
	place := nodewire.Place(nodeKey)
	name := strings.TrimSpace(req.Name)
	if !NameShape.MatchString(strings.ToLower(name)) {
		return fmt.Errorf("名字 %q 不合规：小写字母、数字、点、下划线、连字符", name)
	}
	if reservedMCP(name) {
		return fmt.Errorf("%q 是平台自己用的名字", name)
	}
	entries, err := mcpregistry.Search(ctx, req.Entry, 50)
	if err != nil {
		return err
	}
	var entry *mcpregistry.Entry
	for i := range entries {
		if entries[i].Name == req.Entry {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("注册表里找不到 %q", req.Entry)
	}
	var setting mcpregistry.Setting
	var wants []mcpregistry.EnvVar
	var provenance string
	switch {
	case req.Package != nil:
		if *req.Package < 0 || *req.Package >= len(entry.Packages) {
			return errors.New("没有这个包")
		}
		pkg := entry.Packages[*req.Package]
		setting, err = mcpregistry.Plan(pkg)
		if err != nil {
			return err
		}
		wants = pkg.Env
		if pkg.Needs != "" && !a.machineHasTool(ctx, nodeKey, pkg.Needs) {
			return fmt.Errorf("%s 上没有 %s，装不了这个包；先在那台机器上装好 %s", place, pkg.Needs, pkg.Needs)
		}
		provenance = "registry:" + entry.Name + " " + pkg.RegistryType + ":" + pkg.Identifier
		if pkg.Version != "" {
			provenance += "@" + pkg.Version
		}
		setting.Env = map[string]string{}
		for _, v := range wants {
			val := req.Values[v.Name]
			if val == "" {
				val = v.Default
			}
			if val == "" {
				if v.Required {
					return fmt.Errorf("%s 是必填的", v.Name)
				}
				continue
			}
			setting.Env[v.Name] = val
		}
	case req.Remote != nil:
		if *req.Remote < 0 || *req.Remote >= len(entry.Remotes) {
			return errors.New("没有这个远端")
		}
		remote := entry.Remotes[*req.Remote]
		setting, err = mcpregistry.PlanRemote(remote)
		if err != nil {
			return err
		}
		if err := refusePrivate(ctx, setting.URL); err != nil {
			return err
		}
		wants = remote.Headers
		provenance = "registry:" + entry.Name + " remote"
		setting.Headers = map[string]string{}
		for _, v := range wants {
			val := req.Values[v.Name]
			if val == "" {
				val = v.Default
			}
			if val == "" {
				if v.Required {
					return fmt.Errorf("%s 是必填的", v.Name)
				}
				continue
			}
			setting.Headers[v.Name] = val
		}
	default:
		return errors.New("要选一个包或一个远端")
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	var set nodewire.Settings
	if nodeKey == "" {
		set = a.hubSettings()
	} else if set, err = a.Nodes.Settings(ctx, nodeKey); err != nil {
		return err
	}
	if _, exists := set.MCPServers[name]; exists {
		return fmt.Errorf("%s 上已经有叫 %q 的 MCP 服务器", place, name)
	}
	if set.MCPServers == nil {
		set.MCPServers = map[string]nodewire.MCPSetting{}
	}
	set.MCPServers[name] = nodewire.MCPSetting{Type: setting.Type, Command: setting.Command, Args: setting.Args, Env: setting.Env, URL: setting.URL, Headers: setting.Headers}
	if nodeKey == "" {
		_, err = a.setNodeSettingsLocked(ctx, NodeName(), set)
	} else {
		_, err = a.Nodes.Configure(ctx, nodeKey, set)
	}
	if err != nil {
		return err
	}
	a.remember(place, name, provenance)
	return nil
}

// machineHasTool says whether a machine's snapshot offers a command.
func (a *Service) machineHasTool(ctx context.Context, nodeKey, tool string) bool {
	adv, err := a.advertOf(ctx, nodeKey)
	if err != nil || adv.Snapshot == nil {
		return false
	}
	for _, c := range adv.Snapshot.Offers {
		if c.Kind == ability.Tool && c.ID == tool {
			return true
		}
	}
	return false
}

// refusePrivate keeps a remote from pointing at this machine, a private
// network, a link-local address or a cloud metadata service: a
// registry entry is a stranger's, and the machine's loopback proxy
// would carry the owner's headers wherever it says.
func refusePrivate(ctx context.Context, raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("地址 %q 不合规", raw)
	}
	host := u.Hostname()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(lctx, host)
		if err != nil {
			return fmt.Errorf("解析 %s 失败：%v", host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.Equal(net.ParseIP("169.254.169.254")) || (ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) {
			return fmt.Errorf("%s 指向本机或内网（%s），默认不允许从注册表装到这样的地址", host, ip)
		}
	}
	return nil
}
