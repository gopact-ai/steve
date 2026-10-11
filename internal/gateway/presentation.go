package gateway

import (
	"encoding/json"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/view"
)

func (g *Gateway) render(ch Channel, t view.Turn, copy view.Copy) []byte {
	if p, ok := ch.(channel.Presenter); ok {
		return p.Render(t, copy)
	}
	// An adapter without a rich presenter must refuse ReplyCard. This neutral
	// image is useful to adapters that transport structured turn snapshots.
	raw, _ := json.Marshal(t)
	return raw
}
