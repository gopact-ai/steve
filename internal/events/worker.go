package events

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/work"
)

// Pump makes one bounded pass. More than one owner can call it: atomic fanout,
// claim tokens and the Work service's stable IDs decide who advances a record.
func (s *Service) Pump(ctx context.Context, intelligence *work.Service) error {
	var failures []error
	ids, err := s.scan.NextStates(ctx, s.Book, DeliveryKind, []string{StatePending}, 32)
	if err != nil {
		failures = append(failures, err)
	}
	for _, id := range ids {
		if err := s.RetirePending(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	for _, phase := range []struct {
		kind string
		run  func(string) error
	}{
		{OutboxKind, func(id string) error { return s.Route(ctx, id) }},
		{ActiveKind, func(id string) error { return s.RecoverClaim(ctx, id) }},
		{ReadyKind("work"), func(id string) error { return s.runWork(ctx, intelligence, id) }},
		{TrackingKind, func(id string) error { return s.trackWork(ctx, intelligence, id) }},
	} {
		ids, err := s.scan.Next(ctx, s.Book, phase.kind, 32)
		if err != nil {
			failures = append(failures, err)
		}
		for _, id := range ids {
			if err := phase.run(id); err != nil && !errors.Is(err, ledger.ErrConflict) && !errors.Is(err, ledger.ErrStale) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) runWork(ctx context.Context, intelligence *work.Service, id string) error {
	claimed, err := s.claim(ctx, identity.Principal{Owner: true}, id)
	if err != nil {
		return err
	}
	if claimed.Delivery.ID == "" {
		return nil
	}
	raw, err := json.Marshal(claimed.Event)
	if err != nil {
		return err
	}
	record, err := intelligence.Submit(ctx, identity.Principal{Owner: true}, work.Request{Key: "event:" + id, Conversation: claimed.Delivery.Rule.Conversation, Project: claimed.Delivery.Rule.Project, Instruction: claimed.Delivery.Rule.Instruction, Context: raw, Causation: id})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.Book.Update(ctx, func(tx *ledger.Tx) error {
			d, found, e := durable.Get[Delivery](tx, DeliveryKind, id)
			if e != nil {
				return e
			}
			if !found || d.State != StateClaimed || d.Token != claimed.Delivery.Token {
				return ledger.ErrStale
			}
			d.State = StateFailed
			d.Detail = err.Error()
			return saveDelivery(tx, d)
		})
	}
	return s.AcceptWork(ctx, id, claimed.Delivery.Token, record.ID)
}

func (s *Service) trackWork(ctx context.Context, intelligence *work.Service, id string) error {
	d, found, err := durable.Read[Delivery](ctx, s.Book, DeliveryKind, id)
	if err != nil || !found {
		return err
	}
	if d.State != StateAccepted {
		return nil
	}
	record, result, err := intelligence.Status(ctx, identity.Principal{Owner: true}, d.Receipt)
	if err != nil {
		return s.FinishWork(ctx, id, StateUnknown, err.Error())
	}
	if record.State == work.StateFailed {
		return s.FinishWork(ctx, id, StateFailed, record.Detail)
	}
	switch result.State {
	case StateDone:
		return s.FinishWork(ctx, id, StateDone, "")
	case StateFailed, StateCancelled:
		return s.FinishWork(ctx, id, StateFailed, result.Error)
	}
	return nil
}
