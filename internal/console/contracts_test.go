package console

import "github.com/gopact-ai/steve/internal/turn"

// The only production implementation of capabilities the console probes
// for.
var (
	_ neverAdmittedDriver = (*turn.Coordinator)(nil)
	_ relocationDriver    = (*turn.Coordinator)(nil)
	_ retainedPlanDriver  = (*turn.Coordinator)(nil)
	_ retainedProber      = (*turn.Coordinator)(nil)
	_ retainedStopDriver  = (*turn.Coordinator)(nil)
	_ verbLister          = (*turn.Coordinator)(nil)
)
