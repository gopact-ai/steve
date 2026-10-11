package events

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

const ActiveKind = "event-delivery-active"
const TrackingKind = "event-work-tracking"

func ReadyKind(handler string) string { return "event-delivery-ready:" + handler }
func deliveryQueue(d Delivery) string {
	if d.Rule.Mode == "work" {
		return ReadyKind("work")
	}
	return ReadyKind(d.Rule.Handler)
}

type Claimed struct {
	Delivery Delivery `json:"delivery"`
	Event    Event    `json:"event"`
}
type Acknowledgement struct {
	Token   string `json:"claim_token"`
	State   string `json:"state"`
	Receipt string `json:"receipt,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func saveDelivery(tx *ledger.Tx, d Delivery) error {
	d.UpdatedAt = time.Now().UTC()
	if err := tx.PutBinding(DeliveryKind, d.ID, d); err != nil {
		return err
	}
	for _, kind := range []string{deliveryQueue(d), ActiveKind, TrackingKind} {
		if err := durable.Delete(tx, kind, d.ID); err != nil {
			return err
		}
	}
	switch d.State {
	case StatePending:
		return tx.PutBinding(deliveryQueue(d), d.ID, d.ID)
	case StateClaimed:
		return tx.PutBinding(ActiveKind, d.ID, d.ID)
	case StateAccepted:
		return tx.PutBinding(TrackingKind, d.ID, d.ID)
	case StateFailed, StateUnknown:
		return putInbox(tx, d.EventID, d.ID, d.State, d.Detail)
	}
	return nil
}

func (s *Service) claim(ctx context.Context, p identity.Principal, id string) (out Claimed, err error) {
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("delivery not found")
		}
		if d.Rule.Mode == "provider" && grant.ID != d.Rule.Handler || d.Rule.Mode == "work" && !p.Owner || d.Rule.Mode == "inbox" {
			return identity.ErrDenied
		}
		if d.State != StatePending {
			return ledger.ErrConflict
		}
		current, found, err := durable.Get[Subscription](tx, SubscriptionKind, d.Rule.ID)
		if err != nil {
			return err
		}
		if !found || !current.Enabled {
			d.State = StateCancelled
			return saveDelivery(tx, d)
		}
		d.State = StateClaimed
		d.Token = identity.RandomID()
		d.Incarnation = s.Book.Incarnation()
		d.Runtime = s.Runtime
		d.ClaimUntil = time.Now().UTC().Add(time.Minute)
		if err := saveDelivery(tx, d); err != nil {
			return err
		}
		event, found, err := durable.Get[Event](tx, EventKind, d.EventID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("delivery event missing")
		}
		out = Claimed{Delivery: d, Event: event}
		return nil
	})
	return
}

// Poll only claims this provider's deliveries. Returned IDs/tokens are durable
// dispatch evidence. The same ID must be used for downstream action deduplication.
func (s *Service) Poll(ctx context.Context, p identity.Principal) (Claimed, bool, error) {
	var grant identity.Grant
	if err := s.Book.Read(ctx, func(tx *ledger.ReadTx) error { var err error; grant, err = s.Identity.Check(tx, p); return err }); err != nil {
		return Claimed{}, false, err
	}
	if grant.Kind != "provider" {
		return Claimed{}, false, identity.ErrDenied
	}
	ids, _, err := durable.List[string](ctx, s.Book, ReadyKind(grant.ID), "", 32)
	if err != nil {
		return Claimed{}, false, err
	}
	for _, id := range ids {
		out, err := s.claim(ctx, p, id)
		if errors.Is(err, ledger.ErrConflict) {
			continue
		}
		if err != nil {
			return out, false, err
		}
		if out.Delivery.ID != "" {
			return out, true, nil
		}
	}
	return Claimed{}, false, nil
}

func (s *Service) Ack(ctx context.Context, p identity.Principal, id string, ack Acknowledgement) error {
	if ack.State != StateDone && ack.State != StateFailed && ack.State != StateUnknown || ack.State == StateDone && ack.Receipt == "" || len(ack.Detail) > 4096 || len(ack.Receipt) > 4096 {
		return errors.New("consumer acknowledgement requires done/failed/unknown and a bounded receipt for done")
	}
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found || d.Rule.Mode != "provider" || d.Rule.Handler != grant.ID {
			return identity.ErrDenied
		}
		if ack.Token == "" || d.Token != ack.Token || d.Incarnation != s.Book.Incarnation() {
			return ledger.ErrStale
		}
		if d.State == StateDone || d.State == StateFailed {
			if d.State == ack.State && d.Receipt == ack.Receipt && d.Detail == ack.Detail {
				return nil
			}
			return ledger.ErrConflict
		}
		// Late evidence for the exact original claim can resolve uncertainty. Once
		// an operator retries, Token changes and that old result is refused.
		if d.State != StateClaimed && d.State != StateUnknown {
			return ledger.ErrConflict
		}
		d.State = ack.State
		d.Receipt = ack.Receipt
		d.Detail = ack.Detail
		return saveDelivery(tx, d)
	})
}

func (s *Service) RecoverClaim(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil || !found {
			return err
		}
		if d.State != StateClaimed {
			return nil
		}
		if d.Runtime == s.Runtime && d.Incarnation == s.Book.Incarnation() && d.ClaimUntil.After(time.Now()) {
			return nil
		}
		if d.Rule.Mode == "work" || d.Rule.ReplaySafe {
			d.State = StatePending
			d.Token = ""
		} else {
			d.State = StateUnknown
			d.Detail = "Consumer claim expired without a receipt; reconcile the original action before retrying"
		}
		return saveDelivery(tx, d)
	})
}

// Retry is an explicit operator decision. Frozen rule and event remain intact;
// a new claim token rejects results from the interrupted consumer invocation.
func (s *Service) Retry(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found || d.State != StateFailed && d.State != StateUnknown {
			return ledger.ErrConflict
		}
		d.State = StatePending
		d.Token = ""
		d.Receipt = ""
		d.Detail = ""
		return saveDelivery(tx, d)
	})
}

func (s *Service) AcceptWork(ctx context.Context, id, token, workID string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found || d.State != StateClaimed || d.Token != token || d.Incarnation != s.Book.Incarnation() {
			return ledger.ErrStale
		}
		d.State = StateAccepted
		d.Receipt = workID
		return saveDelivery(tx, d)
	})
}

func (s *Service) FinishWork(ctx context.Context, id, state, detail string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		d, found, err := durable.Get[Delivery](tx, DeliveryKind, id)
		if err != nil {
			return err
		}
		if !found || d.Rule.Mode != "work" {
			return ledger.ErrConflict
		}
		if d.State != StateAccepted {
			return nil
		}
		d.State = state
		d.Detail = detail
		return saveDelivery(tx, d)
	})
}
