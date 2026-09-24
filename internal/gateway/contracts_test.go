package gateway

import "github.com/gopact-ai/steve/internal/channel/feishu"

// The only production channel.
var _ Channel = (*feishu.Channel)(nil)
