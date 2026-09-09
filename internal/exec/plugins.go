package exec

import (
	"context"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (r *stepRun) PreparePluginSession(ctx context.Context, req harness.PluginPreparation) (*plugins.RuntimeRef, error) {
	if provider, ok := r.deps.Runner.(harness.PluginSessionPreparer); ok {
		return provider.PreparePluginSession(ctx, req)
	}
	if req.Prior != nil {
		return nil, plugins.ErrUnavailable
	}
	return nil, nil
}

func (a *AgentRunner) PreparePluginSession(ctx context.Context, req harness.PluginPreparation) (*plugins.RuntimeRef, error) {
	if provider, ok := a.sessions.(harness.PluginSessionPreparer); ok {
		return provider.PreparePluginSession(ctx, req)
	}
	if req.Prior != nil {
		return nil, plugins.ErrUnavailable
	}
	return nil, nil
}
