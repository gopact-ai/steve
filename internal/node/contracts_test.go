package node

import (
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/harness"
)

// The only production implementation of capabilities artifact and harness
// probe for.
var (
	_ artifact.DirectNodes         = (*Registry)(nil)
	_ harness.NodeSessionTransport = (*Registry)(nil)
	_ harness.PluginTransports     = (*Registry)(nil)
)
