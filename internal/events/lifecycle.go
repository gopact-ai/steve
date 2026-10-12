package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/provider"
)

var ErrConsumerUnavailable = errors.New("event consumer is unavailable")

func resolveInboxTx(tx *ledger.Tx, event, delivery, reason string) error {
	id := durable.Key(event, delivery, reason)
	inbox, found, err := durable.Get[Inbox](tx, InboxKind, id)
	if err != nil || !found {
		return err
	}
	if inbox.State != StateResolved {
		inbox.State = StateResolved
		if err := tx.PutBinding(InboxKind, id, inbox); err != nil {
			return err
		}
	}
	return durable.Delete(tx, InboxOpenKind, id)
}

func (s *Service) availableConsumerTx(tx ledger.Reader, d Delivery) error {
	rule, found, err := durable.Get[Subscription](tx, SubscriptionKind, d.Rule.ID)
	if err != nil {
		return err
	}
	if !found || !rule.Enabled {
		return fmt.Errorf("%w: subscription is disabled", ErrConsumerUnavailable)
	}
	if d.Rule.Mode != "provider" {
		return nil
	}
	g, found, err := durable.Get[identity.Grant](tx, identity.GrantKind, d.Rule.Handler)
	if err != nil {
		return err
	}
	if !found || !g.Enabled || g.Kind != "provider" {
		return fmt.Errorf("%w: assigned consumer is retired", ErrConsumerUnavailable)
	}
	return nil
}

// RetirePending does not need the consumer to reconnect. Never turn a known
// dispatched or unknown action back into a fresh pending delivery here.
func (s *Service) RetirePending(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil || !found {
			return err
		}
		if d.State != StatePending {
			return nil
		}
		if err := s.availableConsumerTx(tx, d); err != nil {
			if !errors.Is(err, ErrConsumerUnavailable) {
				return err
			}
			d.State = StateCancelled
			d.Detail = err.Error() + " before dispatch; review this event before reprocessing"
			return saveDelivery(tx, d)
		}
		wait := s.WaitAfter
		if wait <= 0 {
			wait = 5 * time.Minute
		}
		health := provider.Health{}
		if d.Rule.Mode == "provider" {
			var e error
			health, e = provider.HealthTx(tx, d.Rule.Handler)
			if e != nil {
				return e
			}
		}
		if health.State == "offline" || !d.UpdatedAt.Add(wait).After(time.Now()) {
			return putInbox(tx, d.EventID, d.ID, "waiting", "This event has not been claimed; check the consumer or subscription")
		}
		return nil
	})
}

type Reconciliation struct {
	Revision uint64 `json:"revision"`
	State    string `json:"state"`
	Receipt  string `json:"receipt"`
	Detail   string `json:"detail"`
}

type Resolution struct {
	Reconciliation
	Owner           string    `json:"owner"`
	At              time.Time `json:"at"`
	OriginalState   string    `json:"original_state"`
	OriginalReceipt string    `json:"original_receipt,omitempty"`
	OriginalDetail  string    `json:"original_detail,omitempty"`
}

// Reconcile records an explicit owner attestation. It does not replace the
// original claim or authorize another effect.
func (s *Service) Reconcile(ctx context.Context, id string, input Reconciliation) error {
	if input.State != StateDone && input.State != StateFailed || input.Receipt == "" || len(input.Receipt) > 4096 || len(input.Detail) > 4096 {
		return errors.New("reconciliation requires a final state and bounded evidence receipt")
	}
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found {
			return ledger.ErrConflict
		}
		if d.Resolution != nil && d.Resolution.Reconciliation == input {
			return nil
		}
		if d.Revision != input.Revision || d.State != StateUnknown {
			return ledger.ErrConflict
		}
		d.Resolution = &Resolution{Reconciliation: input, Owner: s.Identity.OwnerID, At: time.Now().UTC(), OriginalState: d.State, OriginalReceipt: d.Receipt, OriginalDetail: d.Detail}
		d.State = input.State
		d.Receipt = input.Receipt
		d.Detail = "Owner reconciliation: " + input.Detail
		return saveDelivery(tx, d)
	})
}
