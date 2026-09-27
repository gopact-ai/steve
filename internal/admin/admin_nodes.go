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
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
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
	if name == a.NodeName && !a.ClusterMode {
		return a.hubSettings(), nil
	}
	return a.Nodes.Settings(ctx, name)
}

// SetNodeSettings rewrites what a machine offers and answers what is in
// force: on the hub, the config file and the running manager, assembler,
// roster and launch probe; on a node, the node itself.
func (a *Service) SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if name != a.NodeName || a.ClusterMode {
		return a.Nodes.Configure(ctx, name, set)
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.setNodeSettingsLocked(ctx, name, set)
}

// setNodeSettingsLocked is SetNodeSettings for the hub with a.mu held.
func (a *Service) setNodeSettingsLocked(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if set.Revision == "" || set.Revision != a.hubSettings().Revision {
		return nodewire.Settings{}, nodewire.ErrSettingsRevisionConflict
	}
	set = nodewire.CloneSettings(set)
	harnesses, err := a.hubHarnessSettings(textFor(ctx), set.Harnesses)
	if err != nil {
		return nodewire.Settings{}, err
	}
	servers, err := a.hubMCPSettings(textFor(ctx), set.MCPServers)
	if err != nil {
		return nodewire.Settings{}, err
	}

	for _, d := range set.Declares {
		if !strings.Contains(d, ":") {
			return nodewire.Settings{}, textFor(ctx).Errorf(i18n.AdminDeclarationForm, d)
		}
	}
	var previous map[string]config.Harness
	var stateDir string
	saveErr := a.updateConfig(a.lifetime(), func(c *config.Config) error {
		// Agents keep naming harnesses that exist.
		for id, ag := range c.Agents {
			if ag.Node == "" {
				if _, ok := harnesses[ag.Harness]; !ok {
					return textFor(ctx).Errorf(i18n.AdminAgentUsesHarness, id, ag.Harness)
				}
				for _, srv := range ag.MCPServers {
					if _, ok := servers[srv]; !ok {
						return textFor(ctx).Errorf(i18n.AdminAgentUsesMCPServer, id, srv)
					}
				}
			}
		}
		previous = c.Harnesses
		c.Harnesses = harnesses
		c.MCPServers = servers
		c.Gateway.Tools = append([]string(nil), set.Tools...)
		c.Gateway.Declares = append([]string(nil), set.Declares...)
		c.Gateway.Capabilities = append([]string(nil), set.Capabilities...)
		stateDir = filepath.Dir(c.Gateway.StatePath)
		return nil
	})
	if saveErr != nil && !config.Committed(saveErr) {
		if errors.Is(saveErr, config.ErrFileChanged) {
			return nodewire.Settings{}, errors.Join(nodewire.ErrSettingsRevisionConflict, saveErr)
		}
		return nodewire.Settings{}, saveErr
	}
	// The running pieces follow the file.
	for id, h := range harnesses {
		if err := a.Manager.Set(id, harness.Config{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: runtime.ApplyEnv(h.Env, id, stateDir), Permission: h.Permission}); err != nil {
			slog.Error(fmt.Sprintf("steve: harness %s: %v", id, err), "harness", id)
		}
	}
	for id := range previous {
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
	a.ConfigStore.rlock()
	slots := a.cfg().HubSlots()
	a.ConfigStore.runlock()
	a.Fleet.SetHubSlots(slots)
	if a.Observation != nil {
		a.Observation.Launch.Wake()
	}
	slog.Info(fmt.Sprintf("steve: hub settings applied from the page: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(harnesses), len(set.Tools), len(servers), len(set.Declares), len(set.Capabilities)))
	return a.hubSettings(), saveErr
}

func (a *Service) hubHarnessSettings(text i18n.Catalog, settings map[string]nodewire.HarnessSetting) (map[string]config.Harness, error) {
	harnesses := make(map[string]config.Harness, len(settings))
	for id, h := range settings {
		if !NameShape.MatchString(strings.ToLower(id)) || strings.TrimSpace(h.Command) == "" {
			return nil, text.Errorf(i18n.AdminHarnessInvalid, id)
		}
		a.ConfigStore.rlock()
		item := a.cfg().Harnesses[id]
		a.ConfigStore.runlock()
		if h.Adapter != nil && *h.Adapter != item.Adapter {
			return nil, text.Errorf(i18n.AdminHarnessAdapterRestart, id)
		}
		if item.Adapter != "" && h.Command != item.Command {
			return nil, text.Errorf(i18n.AdminHarnessFixedAdapter, id)
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

func (a *Service) hubMCPSettings(text i18n.Catalog, settings map[string]nodewire.MCPSetting) (map[string]config.MCPServer, error) {

	servers := make(map[string]config.MCPServer, len(settings))
	for id, m := range settings {
		a.ConfigStore.rlock()
		old := a.cfg().MCPServers[id]
		a.ConfigStore.runlock()
		if m.Env == nil {
			m.Env = old.Env
		}
		if m.Headers == nil {
			m.Headers = old.Headers
		}
		if !NameShape.MatchString(strings.ToLower(id)) {
			return nil, text.Errorf(i18n.AdminMCPServerNameInvalid, id)
		}
		switch m.Type {
		case "", "stdio":
			if strings.TrimSpace(m.Command) == "" {
				return nil, text.Errorf(i18n.AdminMCPServerNeedsCommand, id)
			}
			m.Type = "stdio"
		case "http", "sse":
			if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
				return nil, text.Errorf(i18n.AdminMCPServerNeedsURL, id)
			}
		default:
			return nil, text.Errorf(i18n.AdminMCPServerUnknownType, id, m.Type)
		}
		servers[id] = config.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	return servers, nil
}

func (a *Service) hubSettings() nodewire.Settings {
	a.ConfigStore.rlock()
	defer a.ConfigStore.runlock()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, a.cfg().Gateway.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, a.cfg().Gateway.Declares...), Capabilities: append([]string{}, a.cfg().Gateway.Capabilities...)}
	for id, h := range a.cfg().Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Adapter: &h.Adapter, Slots: &h.Slots, Permission: &h.Permission, Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir}
	}
	for id, m := range a.cfg().MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	out.Revision = nodewire.SettingsRevision(out)
	return nodewire.CloneSettings(out)
}

func (a *Service) AddNode(ctx context.Context, req consoleapi.AddNodeRequest) (consoleapi.AddNodeResult, error) {
	name := strings.TrimSpace(req.Name)
	if !NameShape.MatchString(name) {
		return consoleapi.AddNodeResult{}, errors.New(textFor(ctx).T(i18n.AdminNodeNameInvalid))
	}
	addr := strings.TrimSpace(req.Addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return consoleapi.AddNodeResult{}, errors.New(textFor(ctx).T(i18n.AdminNodeAddressForm))
	}
	level := datalevel.Level(strings.TrimSpace(req.Level)).OrDefault()
	if _, ok := map[datalevel.Level]bool{datalevel.Public: true, datalevel.Internal: true, datalevel.Restricted: true, datalevel.Sealed: true}[level]; !ok {
		return consoleapi.AddNodeResult{}, errors.New(textFor(ctx).T(i18n.AdminNodeLevelInvalid))
	}
	var raw [24]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	a.Mu.Lock()
	defer a.Mu.Unlock()
	var levels map[string]datalevel.Level
	var regions map[string]string
	var binary string
	saveErr := a.updateConfig(ctx, func(c *config.Config) error {
		if _, exists := c.Nodes[name]; exists {
			return textFor(ctx).Errorf(i18n.AdminNodeExists, name)
		}
		if name == a.NodeName {
			return textFor(ctx).Errorf(i18n.AdminNodeIsHub, name)
		}
		if c.Nodes == nil {
			c.Nodes = map[string]config.Node{}
		}
		c.Nodes[name] = config.Node{Addr: addr, Token: token, Level: string(level)}
		levels, regions, binary = c.NodeLevels(), c.NodeRegions(), c.Gateway.NodeBinary
		return nil
	})
	if saveErr != nil && !config.Committed(saveErr) {
		return consoleapi.AddNodeResult{}, saveErr
	}
	a.hubURL = req.HubURL
	a.Nodes.Add(name, node.Config{Addr: addr, Token: token, Level: string(level)})
	a.Fleet.SetNodeLevels(levels)
	a.Fleet.SetNodeRegions(regions)
	slog.Info(fmt.Sprintf("steve: machine %s added (%s, %s); waiting for it to come up", name, addr, level), "node", name)
	out := consoleapi.AddNodeResult{Name: name, Token: token,
		Command: fmt.Sprintf("curl -fsSL '%s/bootstrap/%s?token=%s' | bash -l", req.HubURL, name, token)}
	if binary == "" {
		out.Note = textFor(ctx).T(i18n.AdminNodeBinaryMissingNote)
	}
	return out, saveErr
}

// AdmitWorker records a cluster worker that has joined and dials it with
// worker's transport. A worker already recorded with the same address and
// token keeps its recorded level; one recorded with a different address or
// token is refused with coordination.ErrConflict. When the configuration
// cannot be saved, nothing is recorded and the worker is not dialed; when
// it is saved but its directory cannot be synced, the worker is recorded
// and dialed, and that error is returned along with any failure to reach
// the worker.
func (a *Service) AdmitWorker(ctx context.Context, nodeID string, worker node.Config) error {
	next := config.Node{Addr: worker.Addr, Token: worker.Token, Level: string(datalevel.Level(worker.Level).OrDefault())}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	var levels map[string]datalevel.Level
	var regions map[string]string
	saveErr := a.updateConfig(a.lifetime(), func(c *config.Config) error {
		existing, known := c.Nodes[nodeID]
		if known && (existing.Addr != next.Addr || existing.Token != next.Token) {
			return coordination.ErrConflict
		}
		if known {
			next = existing
		} else {
			if c.Nodes == nil {
				c.Nodes = map[string]config.Node{}
			}
			c.Nodes[nodeID] = next
		}
		levels, regions = c.NodeLevels(), c.NodeRegions()
		if known {
			return errUnchanged
		}
		return nil
	})
	if saveErr != nil && !config.Committed(saveErr) {
		return saveErr
	}
	worker.Level = next.Level
	a.Nodes.Add(nodeID, worker)
	a.Fleet.SetNodeLevels(levels)
	a.Fleet.SetNodeRegions(regions)
	if _, err := a.Nodes.Refresh(ctx, nodeID); err != nil {
		return errors.Join(saveErr, err)
	}
	return saveErr
}

// RemoveNode forgets a machine. Nothing may still live on it: an agent
// placed there, a project homed there or with a copy there, keep it. A
// machine in the cluster leaves it first; when the cluster refuses, the
// configuration is kept so the machine stays on the page to try again.
func (a *Service) RemoveNode(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == a.NodeName || name == "hub" {
		return textFor(ctx).Errorf(i18n.AdminHubNotRemovable, name)
	}
	// Placement checks and removal share the same administration boundary as
	// agent/project updates; no new owner can arrive between check and delete.
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.Catalog != nil {
		for _, ag := range a.Catalog.List() {
			if ag.Node == name {
				return textFor(ctx).Errorf(i18n.AdminAgentStillOnNode, ag.ID, name)
			}
		}
	}
	if a.Projects != nil {
		list, err := a.Projects.List(ctx)
		if err != nil {
			return textFor(ctx).Errorf(i18n.AdminNodeProjectCheckFailed, name, err)
		}
		for _, p := range list {
			for _, ws := range p.Workspaces() {
				if ws.Node == name {
					still := i18n.AdminProjectHomeStillOn
					if ws.Kind == project.KindCopy {
						still = i18n.AdminProjectCopyStillOn
					}
					return errors.New(textFor(ctx).T(still, p.ID, name))
				}
			}
		}
	}
	a.ConfigStore.rlock()
	_, inConfig := a.cfg().Nodes[name]
	a.ConfigStore.runlock()
	if !inConfig {
		return textFor(ctx).Errorf(i18n.AdminNoNode, name)
	}
	// Leaving the cluster can take a consensus round; readers of the
	// configuration need not wait for it, the administration lock already
	// keeps the machine from gaining new occupants meanwhile.
	if a.Members != nil {
		if err := a.Members.RemoveMember(ctx, name); err != nil {
			return textFor(ctx).Errorf(i18n.AdminNodeNotLeft, name, err)
		}
	}
	var levels map[string]datalevel.Level
	var regions map[string]string
	saveErr := a.updateConfig(ctx, func(c *config.Config) error {
		delete(c.Nodes, name)
		levels, regions = c.NodeLevels(), c.NodeRegions()
		return nil
	})
	if saveErr != nil && !config.Committed(saveErr) {
		return saveErr
	}
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
	a.ConfigStore.rlock()
	n, ok := a.cfg().Nodes[name]
	harnesses := make(map[string]nodebootstrap.Harness, len(a.cfg().Harnesses))
	for id, h := range a.cfg().Harnesses {
		if h.Adapter != "" {
			harnesses[id] = nodebootstrap.Harness{Adapter: h.Adapter}
		} else {
			harnesses[id] = nodebootstrap.Harness{Command: h.Command, Args: append([]string(nil), h.Args...)}
		}
	}
	binary, hubURL := a.cfg().Gateway.NodeBinary, a.hubURL
	a.ConfigStore.runlock()
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
		// Only whether the installer can be sent matters; its reason is not shown.
		metadata, err := nodebootstrap.InspectBinary(i18n.Catalog{}, binary)
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
	a.ConfigStore.rlock()
	defer a.ConfigStore.runlock()
	if a.cfg().Gateway.NodeBinary == "" || token == "" {
		return "", false
	}
	for _, n := range a.cfg().Nodes {
		if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) == 1 {
			return a.cfg().Gateway.NodeBinary, true
		}
	}
	return "", false
}
