package feishu

import (
	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/view"
)

func (*Channel) Render(t view.Turn, copy view.Copy) []byte { return card.Render(t, copy) }
