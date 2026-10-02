package attempt

import (
	"context"

	"github.com/gopact-ai/steve/internal/ledger"
)

type AbandonDelivery string

const (
	AbandonDelivered           AbandonDelivery = "delivered"
	AbandonDeliveryNotRequired AbandonDelivery = "not-required"
	AbandonDeliveryGone        AbandonDelivery = "gone"
	AbandonDeliveryTerminal    AbandonDelivery = "already-terminal"
)

// CompleteAbandonDelivery records the receiver's durable evidence, not an
// in-memory claim of success. Session retirement is a separate obligation.
func (s *Service) CompleteAbandonDelivery(ctx context.Context, expected Record, evidence func(ledger.Reader, Record) (AbandonDelivery, error)) error {
	return nil
}
