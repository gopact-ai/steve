package turn

import (
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/harness"
)

// The only production implementations of capabilities turn probes for.
var (
	_ executionGate        = (*agentmcp.Server)(nil)
	_ originalOpenRecovery = (*harness.Manager)(nil)
	_ pluginRuntimeCloser  = (*harness.Manager)(nil)
	_ retainedRuntime      = (*harness.Manager)(nil)
	_ retainedPlanner      = (*exec.Supervisor)(nil)
	_ retainedRunReader    = (*exec.Supervisor)(nil)
	_ rulePlanner          = (*exec.Supervisor)(nil)
)
