package console

import (
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
)

// The only production implementation of capabilities httpapi probes for.
var (
	_ consoleapi.ExchangeIdentity = (*Service)(nil)
)

// The only production implementation of capabilities console probes its
// handler for.
var (
	_ contextProvider         = (*turn.Coordinator)(nil)
	_ conversationInitializer = (*turn.Coordinator)(nil)
	_ inputParser             = (*turn.Coordinator)(nil)
	_ localizedVerbLister     = (*turn.Coordinator)(nil)
	_ neverAdmittedDriver     = (*turn.Coordinator)(nil)
	_ relocationDriver        = (*turn.Coordinator)(nil)
	_ retainedPlanDriver      = (*turn.Coordinator)(nil)
	_ retainedProber          = (*turn.Coordinator)(nil)
	_ retainedStopDriver      = (*turn.Coordinator)(nil)
	_ sessionResetter         = (*turn.Coordinator)(nil)
	_ setupProvider           = (*turn.Coordinator)(nil)
	_ suggester               = (*turn.Coordinator)(nil)
	_ verbLister              = (*turn.Coordinator)(nil)
)
