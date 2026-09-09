package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/plugins"
)

type PluginRuntimeUsage interface {
	BeginPluginRuntimeUse(context.Context, Placement, plugins.RuntimeRef) error
	EndPluginRuntimeUse(context.Context, Placement, plugins.RuntimeRef) error
}

func (m *Manager) pluginUsage(ctx context.Context, at Placement, ref plugins.RuntimeRef, begin bool) error {
	m.mu.Lock()
	provider := m.pluginRuntimes
	m.mu.Unlock()
	if usage, ok := provider.(PluginRuntimeUsage); ok {
		if begin {
			return usage.BeginPluginRuntimeUse(ctx, at, ref)
		}
		return usage.EndPluginRuntimeUse(ctx, at, ref)
	}
	return nil
}
