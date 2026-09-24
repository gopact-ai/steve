package gateway

import (
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/turn"
)

// The only production channel.
var _ Channel = (*feishu.Channel)(nil)

// The only production implementation of capabilities gateway probes its
// channel for.
var (
	_ cardPoster = (*feishu.Channel)(nil)
)

// The only production implementation of capabilities gateway probes its
// processor for.
var (
	_ RecoveryDriver     = (*turn.Coordinator)(nil)
	_ inputParser        = (*turn.Coordinator)(nil)
	_ scheduledValidator = (*turn.Coordinator)(nil)
)
