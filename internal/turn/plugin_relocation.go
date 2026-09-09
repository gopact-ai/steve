package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (c *Coordinator) planPluginRelocation(ctx context.Context, old attempt.Record, target attempt.Spec) (*plugins.Relocation, error) {
	if old.PluginRuntime == nil {
		return nil, nil
	}
	provider, ok := c.runtime.(harness.PluginRelocationPreparer)
	if !ok {
		return nil, plugins.ErrUnavailable
	}
	return provider.PlanPluginRelocation(ctx, harness.PluginPreparation{AgentID: target.Agent, Project: target.Project, AttemptID: target.ID, At: harness.Placement{Node: target.Node, Harness: target.Harness}, Prior: old.PluginRuntime.Clone()})
}

func (c *Coordinator) prepareRelocationPlugins(ctx context.Context, r attempt.Record, p attempt.RelocationIntent) (*plugins.RuntimeRef, error) {
	if p.Plugins == nil {
		return nil, nil
	}
	provider, ok := c.runtime.(harness.PluginRelocationPreparer)
	if !ok {
		return nil, plugins.ErrUnavailable
	}
	ref, err := provider.PreparePluginRelocation(ctx, p.ID, r.ID, *p.Plugins)
	if err != nil {
		return nil, err
	}
	if ref == nil || ref.Validate() != nil {
		return nil, plugins.ErrIntegrity
	}
	want, err := p.Plugins.Selection.Hash()
	if err != nil {
		return nil, err
	}
	actual, _ := ref.Selection.Hash()
	if want != actual || (r.PluginRuntime != nil && r.PluginRuntime.ID != ref.ID) {
		return nil, errors.New("relocation plugin runtime changed")
	}
	return ref, nil
}
