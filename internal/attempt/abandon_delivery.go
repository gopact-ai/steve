package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type AbandonDelivery string

const (
	AbandonDelivered           AbandonDelivery = "delivered"
	AbandonDeliveryNotRequired AbandonDelivery = "not-required"
	AbandonDeliveryGone        AbandonDelivery = "gone"
	AbandonDeliveryTerminal    AbandonDelivery = "already-terminal"
)

func (d AbandonDelivery) valid() bool {
	switch d {
	case AbandonDelivered, AbandonDeliveryNotRequired, AbandonDeliveryGone, AbandonDeliveryTerminal:
		return true
	}
	return false
}

// CompleteAbandonDelivery records the receiver's durable evidence, not an
// in-memory claim of success. Session retirement is a separate obligation.
func (s *Service) CompleteAbandonDelivery(ctx context.Context, expected Record, evidence func(ledger.Reader, Record) (AbandonDelivery, error)) error {
	if expected.Abandoned == nil || evidence == nil {
		return errors.New("abandonment delivery needs its exact decision and receiver evidence")
	}
	current, err := s.Get(ctx, expected.ID)
	if err != nil {
		return err
	}
	_, err = s.l.Transition(ctx, expected.ID, string(current.State), string(current.State), "abandon-delivery", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var next Record
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if !sameAbandonDeliveryTarget(next, expected) || next.Abandoned.ProjectedAt.IsZero() {
			return ErrForceStopChanged
		}
		if !next.Abandoned.DeliveryDoneAt.IsZero() {
			if !next.Abandoned.DeliveryResult.valid() {
				return errors.New("abandonment delivery has an invalid completion result")
			}
			return errAbandonDelivered
		}
		result, err := evidence(tx, next)
		if err != nil {
			return err
		}
		if !result.valid() {
			return errors.New("abandonment delivery has no receiver completion evidence")
		}
		completed := *next.Abandoned
		completed.DeliveryDoneAt, completed.DeliveryResult = s.now().UTC(), result
		next.Abandoned, next.Revision = &completed, op.Revision+1
		return setRecordDataTx(tx, op, next)
	})
	if errors.Is(err, errAbandonDelivered) {
		return nil
	}
	return err
}

var errAbandonDelivered = errors.New("abandonment delivery was already recorded")

func sameAbandonDeliveryTarget(a, b Record) bool {
	if a.Abandoned == nil || b.Abandoned == nil || a.ID != b.ID || a.TaskID != b.TaskID || a.TurnID != b.TurnID || a.Kind != b.Kind {
		return false
	}
	left, right := *a.Abandoned, *b.Abandoned
	left.ProjectedAt, right.ProjectedAt = time.Time{}, time.Time{}
	left.DeliveryDoneAt, right.DeliveryDoneAt = time.Time{}, time.Time{}
	left.DeliveryResult, right.DeliveryResult = "", ""
	return left == right
}
