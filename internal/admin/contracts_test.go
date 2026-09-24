package admin

import "github.com/gopact-ai/steve/internal/consoleapi"

// The only production implementations of capabilities httpapi probes for.
var (
	_ consoleapi.PluginPresetService       = (*PluginService)(nil)
	_ consoleapi.PluginRemovalService      = (*PluginService)(nil)
	_ consoleapi.PluginRuntimeCloseService = (*PluginService)(nil)
)
