package agentmcp

import "github.com/gopact-ai/steve/internal/intent"

// The only production implementation of capabilities agentmcp probes for.
var (
	_ ExecutionClaims = intent.ForAgents{}
	_ OutcomeReader   = intent.ForAgents{}
)
