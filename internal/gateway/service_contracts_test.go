package gateway

import (
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/work"
)

var _ workContextReader = (*turn.Coordinator)(nil)
var _ work.Conversation = (*Gateway)(nil)
