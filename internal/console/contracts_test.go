package console

import "github.com/gopact-ai/steve/internal/turn"

// The only production implementation of capabilities console recovery
// probes its retained-chat driver for.
var (
	_ neverAdmittedDriver = (*turn.Coordinator)(nil)
	_ relocationDriver    = (*turn.Coordinator)(nil)
	_ retainedPlanDriver  = (*turn.Coordinator)(nil)
	_ retainedProber      = (*turn.Coordinator)(nil)
	_ retainedStopDriver  = (*turn.Coordinator)(nil)
)
