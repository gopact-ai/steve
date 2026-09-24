package artifact

import "github.com/gopact-ai/steve/internal/node"

// The only production implementation of a capability artifact probes for.
var _ directNodes = (*node.Registry)(nil)
