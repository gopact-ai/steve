package work

import "github.com/gopact-ai/steve/internal/agentexec"

// The production implementation of the accounting agentexec probes for.
var _ agentexec.AttemptBudget = Budget{}
