package eventlog

import (
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

func DepthAfter(r ledger.Reader, deliveryID string) (int, error) {
	if deliveryID == "" {
		return 1, nil
	}
	d, found, err := durable.Get[struct {
		EventID string `json:"event_id"`
	}](r, "event-delivery", deliveryID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 8, nil
	}
	e, found, err := durable.Get[Event](r, EventKind, d.EventID)
	if err != nil {
		return 0, err
	}
	if !found || e.Depth >= 7 {
		return 8, nil
	}
	return e.Depth + 1, nil
}
