package node

import "github.com/gopact-ai/steve/internal/harness"

// The only production implementation of capabilities harness probes for.
var (
	_ harness.NodeSessionTransport = (*Registry)(nil)
	_ harness.PluginTransports     = (*Registry)(nil)
)
