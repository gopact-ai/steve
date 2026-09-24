package exec

import (
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/planner"
)

// The only production implementations of capabilities exec probes for.
var (
	_ planResumer        = planner.LLM{}
	_ retainedSessions   = (*harness.Manager)(nil)
	_ retainedStepCloser = (*AgentRunner)(nil)
	_ retainedStepRunner = (*AgentRunner)(nil)
)
