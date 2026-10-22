package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (r *Registry) PluginTransport(node, harnessID string, ref plugins.RuntimeRef) acphost.Transport {
	return remoteTransport{registry: r, node: node, harness: harnessID, plugin: ref.Clone()}
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
	return acphost.LocalTransport{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, ProcessDir: cfg.ProcessDir}, nil
}
