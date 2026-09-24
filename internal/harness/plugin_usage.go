package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/plugins"
)

func (m *Manager) pluginUsage(ctx context.Context, at Placement, ref plugins.RuntimeRef, begin bool) error {
	m.mu.Lock()
	provider := m.pluginRuntimes
	m.mu.Unlock()
	switch {
	case provider == nil:
		return nil
	case begin:
		return provider.BeginPluginRuntimeUse(ctx, at, ref)
	default:
		return provider.EndPluginRuntimeUse(ctx, at, ref)
	}
}
