package roster

import "github.com/gopact-ai/steve/internal/node"

// The only production implementation of a capability roster probes for.
var _ Admitter = (*node.Registry)(nil)
