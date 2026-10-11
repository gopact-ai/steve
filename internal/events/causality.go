package events

import (
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

// DepthAfter follows the recorded delivery rather than trusting a caller's
// depth. Terminal facts are still committed at the limit, but cannot fan out
// into more effects. Missing historical causation is treated conservatively.
func DepthAfter(r ledger.Reader, deliveryID string) (int, error) {
	if deliveryID == "" {
		return 1, nil
	}
	d, found, err := durable.Get[Delivery](r, DeliveryKind, deliveryID)
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
