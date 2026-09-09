package node

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (r *Registry) PluginTransport(node, harnessID string, ref plugins.RuntimeRef) acphost.Transport {
	return remoteTransport{registry: r, node: node, harness: harnessID, plugin: ref.Clone()}
}

func (s *Server) pluginProcessConfig(ctx context.Context, req nodewire.OpenRequest) (acphost.LocalTransport, error) {
	if req.Plugin == nil || req.Plugin.Validate() != nil || req.Plugin.Selection.Node != s.conf().Name || req.Plugin.Selection.Harness != req.Harness {
		return acphost.LocalTransport{}, plugins.ErrInvalid
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
