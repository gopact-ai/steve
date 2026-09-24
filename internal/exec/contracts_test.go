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

// The plugin session preparers exec and lifecycle probe for: a step's Runner
// is an *AgentRunner, and lifecycle opens a step's session through *stepRun.
var (
	_ harness.PluginSessionPreparer = (*AgentRunner)(nil)
	_ harness.PluginSessionPreparer = (*stepRun)(nil)
)
