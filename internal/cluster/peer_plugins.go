package cluster

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (p *Peer) AuthorizePlugins(ctx context.Context, principal string, request nodewire.PluginRequest) error {
	if principal != request.Authority.CoordinatorNodeID || request.Node != p.Config.NodeID {
		return errors.New("plugin request differs from authenticated peer or target")
	}
	return p.authorizePluginRequest(ctx, request)
}

func (p *Peer) ApplicationPluginAuthorizer(active Activation) func(context.Context, string, nodewire.PluginRequest) error {
	return func(ctx context.Context, node string, request nodewire.PluginRequest) error {
		if request.Node != node || request.Authority.CoordinatorNodeID != active.NodeID || request.Authority.CoordinatorEpoch != active.Assignment.Epoch || request.Authority.WriterGeneration != active.WriterGeneration {
			return coordination.ErrStaleEpoch
		}
		if err := active.Context.Err(); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(active.Context, cancel)
		defer stop()
		if err := p.authorizePluginRequest(ctx, request); err != nil {
			return err
		}
		return active.Context.Err()
	}
}

func (p *Peer) authorizePluginRequest(ctx context.Context, req nodewire.PluginRequest) error {
	if req.Authority.ClusterID != p.Config.ClusterID {
		return errors.New("plugin request belongs to another cluster")
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return err
	}
	if state.Coordinator.NodeID != req.Authority.CoordinatorNodeID || state.Coordinator.Epoch != req.Authority.CoordinatorEpoch || state.WriterGeneration != req.Authority.WriterGeneration {
		return coordination.ErrStaleEpoch
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	declaration, found, err := platformconfig.New(runtime.Ledger()).Load()
	if err != nil {
		return err
	}
	if !found {
		return coordination.ErrNotReady
	}
	if _, ok := declaration.Nodes[req.Node]; !ok {
		return errors.New("plugin request targets an unknown node")
	}
	switch req.Action {
	case nodewire.PluginRuntimePrepare, nodewire.PluginRuntimeInspect:
		if req.Action == nodewire.PluginRuntimeInspect && req.Runtime != nil {
			if err := (&plugins.Library{Ledger: runtime.Ledger()}).CheckRuntimeScope(ctx, declaration.Plugins, *req.Runtime); err != nil {
				return err
			}
		}
		return authorizePluginRuntime(declaration, req)
	case nodewire.PluginSecrets:
		return nil
	case nodewire.PluginInspect:
		return req.Deployment.Validate()
	case nodewire.PluginPrepare:
		item, ok := declaration.Plugins[req.Deployment.Installation]
		if !ok {
			return errors.New("plugin installation is not declared")
		}
		expected, err := item.Deployment(req.Deployment.Installation, req.Node)
		if err != nil {
			return err
		}
		want, err := expected.Hash()
		if err != nil {
			return err
		}
		got, err := req.Deployment.Hash()
		if err != nil {
			return err
		}
		if want != got {
			return plugins.ErrConflict
		}
		return nil
	default:
		return plugins.ErrIncompatible
	}
}

func authorizePluginRuntime(declaration platformconfig.Declaration, req nodewire.PluginRequest) error {
	if req.Selection == nil || req.Selection.Node != req.Node {
		return plugins.ErrInvalid
	}
	if _, err := req.Selection.Hash(); err != nil {
		return err
	}
	if req.Action == nodewire.PluginRuntimeInspect {
		if req.Runtime == nil {
			return plugins.ErrInvalid
		}
		// Inspection retains an existing runtime; it never grants a new selection.
		expected, err := req.Runtime.Selection.Hash()
		if err != nil {
			return err
		}
		actual, _ := req.Selection.Hash()
		if actual != expected {
			return plugins.ErrInvalid
		}
		return nil
	}
	wanted := map[string]bool{}
	for id, item := range declaration.Plugins {
		if !item.Enabled {
			continue
		}
		d, err := item.Deployment(id, req.Node)
		if err != nil {
			continue
		}
		if slices.Contains(d.Projects, req.Selection.Project) {
			hash, err := d.Hash()
			if err != nil {
				return err
			}
			wanted[hash] = true
		}
	}
	for _, hash := range req.Selection.Deployments {
		if !wanted[hash] {
			return errors.New("runtime selection is not enabled by current configuration")
		}
	}
	return nil
}
