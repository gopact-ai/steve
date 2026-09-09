package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

// applicationPlugins joins declared project scopes to physical preparation.
// Package/credential data stay in their existing stores; a session receives
// only its durable runtime reference and node-local loopback capabilities.
type applicationPlugins struct {
	gate      *sync.RWMutex
	cfg       *config.Config
	library   *plugins.Library
	local     *node.PluginRuntimePool
	nodes     *node.Registry
	authority nodewire.SessionAuthority
}

func (p *applicationPlugins) PreparePluginSession(ctx context.Context, req harness.PluginPreparation) (*plugins.RuntimeRef, error) {
	if p.gate != nil {
		p.gate.RLock()
		defer p.gate.RUnlock()
	}
	if req.AttemptID == "" || req.Project == "" {
		return nil, plugins.ErrInvalid
	}
	if req.Prior != nil {
		ref := req.Prior
		adminsvc.ConfigMu.RLock()
		items := config.ClonePluginInstallations(p.cfg.Plugins)
		adminsvc.ConfigMu.RUnlock()
		if err := p.library.CheckRuntimeScope(ctx, items, *ref); err != nil {
			return nil, err
		}
		if ref.Selection.Project != req.Project || ref.Selection.Node != req.At.Node || ref.Selection.Harness != req.At.Harness {
			return nil, plugins.ErrInvalid
		}
		if _, _, err := p.PluginRuntime(ctx, req.At, *ref); err != nil {
			return nil, err
		}
		return ref.Clone(), nil
	}
	// Enabling a package affects new sessions; an existing ordinary session
	// cannot silently gain tools that were absent when it was created.
	if req.Upstream != "" {
		if strings.HasPrefix(req.Upstream, "ps_") {
			return nil, plugins.ErrIntegrity
		}
		return nil, nil
	}
	adminsvc.ConfigMu.RLock()
	items := config.ClonePluginInstallations(p.cfg.Plugins)
	cfg := p.cfg.Harnesses[req.At.Harness]
	origin := p.cfg.Agents[req.AgentID].PluginOrigin.Clone()
	adminsvc.ConfigMu.RUnlock()
	selection := plugins.Selection{Project: req.Project, Node: req.At.Node, Harness: req.At.Harness}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		item := items[id]
		if !item.Enabled || !slices.Contains(item.Projects, req.Project) {
			continue
		}
		if _, ok := item.Targets[req.At.Node]; !ok {
			continue
		}
		deployment, err := item.Deployment(id, req.At.Node)
		if err != nil {
			return nil, err
		}
		if err := p.prepareDeployment(ctx, deployment, req.Project); err != nil {
			return nil, err
		}
		hash, err := deployment.Hash()
		if err != nil {
			return nil, err
		}
		selection.Deployments = append(selection.Deployments, hash)
	}
	if origin != nil {
		item, exists := items[origin.Installation]
		_, targeted := item.Targets[req.At.Node]
		if exists && targeted && item.Enabled && slices.Contains(item.Projects, req.Project) {
			selection.Filters = map[string]plugins.CapabilityFilter{origin.Installation: {Skills: origin.Template.Skills, MCP: origin.Template.MCPServers}}
			selection.ExcludedSkills = origin.Adopted.ExcludedSkills()
		}
	}
	if len(selection.Deployments) == 0 {
		return nil, nil
	}
	if err := p.library.ReserveRuntime(ctx, req.AttemptID, selection); err != nil {
		return nil, err
	}
	command := pluginRuntimeCommand(req.AttemptID)
	if req.At.Node == "" {
		runtime, err := p.local.Prepare(ctx, command, selection, harness.Config{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, ProcessDir: cfg.ProcessDir, Permission: cfg.Permission})
		if err != nil {
			return nil, err
		}
		if err := p.library.BindRuntime(ctx, req.AttemptID, runtime.Ref); err != nil {
			return nil, err
		}
		return runtime.Ref.Clone(), nil
	}
	reply, err := p.nodes.Plugins(ctx, req.At.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Permission: cfg.Permission, Authority: p.authority, CommandID: command, Selection: &selection})
	if err != nil {
		return nil, err
	}
	if err := p.library.BindRuntime(ctx, req.AttemptID, *reply.Runtime); err != nil {
		return nil, err
	}
	return reply.Runtime.Clone(), nil
}

func (p *applicationPlugins) prepareDeployment(ctx context.Context, d plugins.Deployment, project string) error {
	bundle, err := p.library.Get(ctx, project, d.Digest)
	if err != nil {
		return err
	}
	if d.Node == "" {
		receipt, err := p.local.Store.PrepareDeployment(ctx, d, plugins.Environment{})
		if err != nil {
			return err
		}
		return p.library.RecordDeployment(ctx, receipt)
	}
	reply, err := p.nodes.Plugins(ctx, d.Node, nodewire.PluginRequest{Action: nodewire.PluginPrepare, Authority: p.authority, Deployment: d, Bundle: bundle.Data})
	if err != nil {
		return err
	}
	return p.library.RecordDeployment(ctx, *reply.Receipt)
}

func (p *applicationPlugins) PluginRuntime(ctx context.Context, at harness.Placement, ref plugins.RuntimeRef) (harness.Config, []acp.MCPServer, error) {
	if ref.Validate() != nil || ref.Selection.Node != at.Node || ref.Selection.Harness != at.Harness {
		return harness.Config{}, nil, plugins.ErrInvalid
	}
	adminsvc.ConfigMu.RLock()
	items := config.ClonePluginInstallations(p.cfg.Plugins)
	adminsvc.ConfigMu.RUnlock()
	if err := p.library.CheckRuntimeScope(ctx, items, ref); err != nil {
		return harness.Config{}, nil, err
	}
	if at.Node == "" {
		runtime, err := p.local.Load(ctx, ref)
		return runtime.Config, runtime.Servers, err
	}
	reply, err := p.nodes.Plugins(ctx, at.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeInspect, Authority: p.authority, Selection: &ref.Selection, Runtime: &ref})
	if err != nil {
		return harness.Config{}, nil, err
	}
	return harness.Config{Permission: reply.Permission, PluginInstructions: reply.Instructions}, reply.Servers, nil
}

func pluginRuntimeCommand(attemptID string) string {
	sum := sha256.Sum256([]byte("runtime/" + attemptID))
	return hex.EncodeToString(sum[:])
}

func assemblePlugins(life lifetime, boot runtimeAssembly, machines fleetAssembly, page consoleAssembly, input inputAssembly) error {
	store := &plugins.Store{Dir: filepath.Join(filepath.Dir(boot.Config().Gateway.StatePath), "plugins")}
	library := &plugins.Library{Store: store, Ledger: boot.Book()}
	pool := &node.PluginRuntimePool{Store: store, StateDir: filepath.Dir(boot.Config().Gateway.StatePath)}
	gate := &sync.RWMutex{}
	service := &applicationPlugins{gate: gate, cfg: boot.Config(), library: library, local: pool, nodes: machines.Nodes()}
	if environment := input.Environment(); environment != nil {
		library.Replication = environment.Content
		service.authority = environment.PluginAuthority
	}
	boot.Manager().SetPluginRuntimes(service)
	control := &adminsvc.PluginService{RuntimeGate: gate, Admin: page.Admin(), Library: library, Local: pool, Authority: service.authority}
	page.Admin().PluginLibrary = library
	page.Dashboard().SetPlugins(control)
	boot.Background().Go(func(ctx context.Context) {
		control.Reconcile(ctx)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				control.Reconcile(ctx)
			}
		}
	})
	life.AfterClose(pool.Close)
	return nil
}

func (p *applicationPlugins) ClosePluginRuntime(ctx context.Context, at harness.Placement, id string) error {
	if at.Node == "" {
		info, err := p.local.Store.RuntimeInfo(id)
		if err != nil {
			return err
		}
		if err := p.local.Store.EndRuntimeUse(ctx, info.Ref, "host/"+id); err != nil {
			return err
		}
		return p.local.Drop(id)
	}
	// A remote broker is closed by the node when its process is known stopped;
	// the runtime directory and its references remain available for restoration.
	return nil
}

func (p *applicationPlugins) BeginPluginRuntimeUse(ctx context.Context, at harness.Placement, ref plugins.RuntimeRef) error {
	if at.Node != "" {
		return nil
	}
	return p.local.Store.BeginRuntimeUse(ctx, ref, "host/"+ref.ID, "host")
}
func (p *applicationPlugins) EndPluginRuntimeUse(ctx context.Context, at harness.Placement, ref plugins.RuntimeRef) error {
	if at.Node != "" {
		return nil
	}
	return p.local.Store.EndRuntimeUse(ctx, ref, "host/"+ref.ID)
}
