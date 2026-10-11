package feishu

import "github.com/gopact-ai/steve/internal/channel"

var _ channel.Presenter = (*Channel)(nil)

var _ channel.ConversationTransport = (*Channel)(nil)
