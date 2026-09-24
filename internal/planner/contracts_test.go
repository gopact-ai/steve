package planner

import "github.com/gopact-ai/steve/internal/agentexec"

// The only production implementation of a capability planner probes for.
var _ retainedExecutor = (*agentexec.Runner)(nil)
