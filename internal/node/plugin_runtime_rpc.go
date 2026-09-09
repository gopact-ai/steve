package node

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (s *Server) pluginRuntimeOperation(ctx context.Context, req nodewire.PluginRequest) (nodewire.PluginReply, error) {
	if req.Selection == nil || req.Selection.Node != s.conf().Name {
		return nodewire.PluginReply{}, plugins.ErrInvalid
	}
	if _, err := req.Selection.Hash(); err != nil {
		return nodewire.PluginReply{}, err
	}
	pool := s.pluginRuntimePool()
	var runtime PluginRuntime
	var err error
	switch req.Action {
	case nodewire.PluginRuntimePrepare:
		policy := req.Permission
		if policy == "" {
			policy = permission.PolicyRead
		}
		if _, err := permission.New(policy); err != nil {
			return nodewire.PluginReply{}, err
		}
		spec, ok := s.conf().Harnesses[req.Selection.Harness]
		if !ok || spec.Command == "" {
			return nodewire.PluginReply{}, fmt.Errorf("%w: harness is not registered", plugins.ErrUnavailable)
		}
		runtime, err = pool.Prepare(ctx, req.CommandID, *req.Selection, harness.Config{Command: spec.Command, Args: spec.Args, Env: spec.Env, ProcessDir: s.processDir(spec), Permission: policy})
	case nodewire.PluginRuntimeInspect:
		if req.Runtime == nil {
			return nodewire.PluginReply{}, plugins.ErrInvalid
		}
		want, _ := req.Selection.Hash()
		actual, e := req.Runtime.Selection.Hash()
		if e != nil || want != actual {
			return nodewire.PluginReply{}, plugins.ErrInvalid
		}
		runtime, err = pool.Load(ctx, *req.Runtime)
	default:
		return nodewire.PluginReply{}, plugins.ErrInvalid
	}
	if err != nil {
		return nodewire.PluginReply{}, err
	}
	return nodewire.PluginReply{Permission: runtime.Config.Permission, Runtime: &runtime.Ref, Servers: runtime.Servers, Instructions: runtime.Instructions}, nil
}
