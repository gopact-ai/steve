package agentexec

import "github.com/gopact-ai/steve/internal/harness"

// The only production implementation of a capability agentexec probes for.
var _ retainedSessions = (*harness.Manager)(nil)
