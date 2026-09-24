package gateway

import (
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/turn"
)

// The only production channel.
var _ Channel = (*feishu.Channel)(nil)

// The only production implementation of the capability gateway probes
// its processor for.
var _ RecoveryDriver = (*turn.Coordinator)(nil)
