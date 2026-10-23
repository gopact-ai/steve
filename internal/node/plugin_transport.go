package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

func (r *Registry) PluginTransport(node, harnessID string, ref plugins.RuntimeRef) acphost.Transport {
	return remoteTransport{registry: r, node: node, harness: harnessID, plugin: ref.Clone()}
}

// PolicyPluginTransport is PluginTransport for a process run under policy.
func (r *Registry) PolicyPluginTransport(node, harnessID string, ref plugins.RuntimeRef, policy string) acphost.Transport {
	return remoteTransport{registry: r, node: node, harness: harnessID, plugin: ref.Clone(), permission: policy}
}

func (s *Server) pluginProcessConfig(ctx context.Context, req nodewire.OpenRequest) (acphost.LocalTransport, error) {
	if req.Plugin == nil {
		return acphost.LocalTransport{}, fmt.Errorf("%w: plugin process request names no runtime", plugins.ErrInvalid)
	}
	if err := req.Plugin.Validate(); err != nil {
		return acphost.LocalTransport{}, err
	}
	if req.Plugin.Selection.Node != s.conf().Name || req.Plugin.Selection.Harness != req.Harness {
		return acphost.LocalTransport{}, fmt.Errorf("%w: plugin runtime %s is for %q/%s, not %q/%s", plugins.ErrInvalid, req.Plugin.ID, req.Plugin.Selection.Node, req.Plugin.Selection.Harness, s.conf().Name, req.Harness)
	}
	if s.conf().SessionAuthorizer != nil {
		return acphost.LocalTransport{}, errors.New("plugin execution on a coordinated node requires a node-owned session")
	}
	runtime, err := s.pluginRuntimePool().Load(ctx, *req.Plugin)
	if err != nil {
		return acphost.LocalTransport{}, err
	}
	cfg := runtime.Config
	env, err := steveruntime.ForPolicy(cfg.Env, req.Harness, streamPolicy(req))
	if err != nil {
		return acphost.LocalTransport{}, err
	}
	return acphost.LocalTransport{Command: cfg.Command, Args: cfg.Args, Env: env, ProcessDir: cfg.ProcessDir}, nil
}
