package app

import (
	"context"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (p *applicationPlugins) PlanPluginRelocation(ctx context.Context, req harness.PluginPreparation) (*plugins.Relocation, error) {
	if req.Prior == nil || req.Prior.Selection.Project != req.Project || req.At.Node == "" {
		return nil, plugins.ErrInvalid
	}
	adminsvc.ConfigMu.RLock()
	items := config.ClonePluginInstallations(p.cfg.Plugins)
	adminsvc.ConfigMu.RUnlock()
	return p.library.PlanRelocation(ctx, items, *req.Prior, req.At.Node, req.At.Harness)
}

func (p *applicationPlugins) PreparePluginRelocation(ctx context.Context, plan, attemptID string, frozen plugins.Relocation) (*plugins.RuntimeRef, error) {
	if p.gate != nil {
		p.gate.RLock()
		defer p.gate.RUnlock()
	}
	adminsvc.ConfigMu.RLock()
	items := config.ClonePluginInstallations(p.cfg.Plugins)
	cfg := p.cfg.Harnesses[frozen.Selection.Harness]
	adminsvc.ConfigMu.RUnlock()
	if err := frozen.CheckScope(items); err != nil {
		return nil, err
	}
	if frozen.Selection.Node == "" || plan == "" || attemptID == "" {
		return nil, plugins.ErrInvalid
	}
	for _, d := range frozen.Deployments {
		bundle, err := p.library.Get(ctx, frozen.Selection.Project, d.Digest)
		if err != nil {
			return nil, err
		}
		if err := p.coordinator(d.Node); err != nil {
			return nil, err
		}
		reply, err := p.nodes.Plugins(ctx, d.Node, nodewire.PluginRequest{Action: nodewire.PluginPrepare, Authority: p.authority, RelocationPlan: plan, Deployment: d, Bundle: bundle.Data})
		if err != nil {
			return nil, err
		}
		if err := p.library.RecordDeployment(ctx, *reply.Receipt); err != nil {
			return nil, err
		}
	}
	if err := p.library.ReserveRuntime(ctx, attemptID, frozen.Selection); err != nil {
		return nil, err
	}
	if err := p.coordinator(frozen.Selection.Node); err != nil {
		return nil, err
	}
	reply, err := p.nodes.Plugins(ctx, frozen.Selection.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Authority: p.authority, RelocationPlan: plan, Permission: cfg.Permission, CommandID: pluginRuntimeCommand(attemptID), Selection: &frozen.Selection})
	if err != nil {
		return nil, err
	}
	if err := p.library.BindRuntime(ctx, attemptID, *reply.Runtime); err != nil {
		return nil, err
	}
	return reply.Runtime.Clone(), nil
}
