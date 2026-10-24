package events

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

// WorkRequest is the work a claimed delivery asks for. Key is stable per
// delivery, so a repeated submission must resolve to the same work.
type WorkRequest struct {
	Key          string
	Conversation string
	Project      string
	Instruction  string
	Context      json.RawMessage
	Causation    string
}

// WorkStatus reports submitted work. Failed means the work could not run at
// all; otherwise State is the outcome (StateDone, StateFailed, StateCancelled)
// or any other value while it is still running.
type WorkStatus struct {
	Failed bool
	Detail string
	State  string
	Error  string
}

// Worker runs the work that deliveries ask for. The task service implements
// it; events stays independent of the product layer.
type Worker interface {
	SubmitWork(ctx context.Context, p identity.Principal, r WorkRequest) (receipt string, err error)
	WorkStatus(ctx context.Context, p identity.Principal, receipt string) (WorkStatus, error)
}

// Pump makes one bounded pass. More than one owner can call it: atomic fanout,
// claim tokens and the Worker's stable keys decide who advances a record.
func (s *Service) Pump(ctx context.Context, intelligence Worker) error {
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

func (s *Service) runWork(ctx context.Context, intelligence Worker, id string) error {
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
	receipt, err := intelligence.SubmitWork(ctx, identity.Principal{Owner: true}, WorkRequest{Key: "event:" + id, Conversation: claimed.Delivery.Rule.Conversation, Project: claimed.Delivery.Rule.Project, Instruction: claimed.Delivery.Rule.Instruction, Context: raw, Causation: id})
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
	return s.AcceptWork(ctx, id, claimed.Delivery.Token, receipt)
}

func (s *Service) trackWork(ctx context.Context, intelligence Worker, id string) error {
	d, found, err := durable.Read[Delivery](ctx, s.Book, DeliveryKind, id)
	if err != nil || !found {
		return err
	}
	if d.State != StateAccepted {
		return nil
	}
	status, err := intelligence.WorkStatus(ctx, identity.Principal{Owner: true}, d.Receipt)
	if err != nil {
		return s.FinishWork(ctx, id, StateUnknown, err.Error())
	}
	if status.Failed {
		return s.FinishWork(ctx, id, StateFailed, status.Detail)
	}
	switch status.State {
	case StateDone:
		return s.FinishWork(ctx, id, StateDone, "")
	case StateFailed, StateCancelled:
		return s.FinishWork(ctx, id, StateFailed, status.Error)
	}
	return nil
}
