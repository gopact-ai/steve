package delegate

import (
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/harness"
)

// The only production implementations of capabilities delegate probes for.
var (
	_ executionBinder = (*agentmcp.Server)(nil)
	_ retainedRuntime = (*harness.Manager)(nil)
)
