package app

import (
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/harness"
)

// The only production implementations of capabilities app and agentexec
// probe for.
var (
	_ agentexec.AttemptBudget = taskBudget{}
	_ applicationOpenRecovery = (*harness.Manager)(nil)
)
