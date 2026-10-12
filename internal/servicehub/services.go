// Package servicehub owns the service modules' lifetime and recovery pump.
// Transports call the narrow module APIs; only composition has access to all
// modules. ACP execution and native channel recovery remain their existing owners.
package servicehub

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/provider"
	"github.com/gopact-ai/steve/internal/work"
)

type Services struct {
	Book      *ledger.Ledger
	Identity  *identity.Service
	Providers *provider.Service
	Work      *work.Service
	Events    *events.Service
	Clients   *clientcap.Broker
	scan      durable.Scanner
	Retention RetentionPolicy
	retention retentionState
}

func New(book *ledger.Ledger, owner string, conversations work.Conversation) *Services {
	identities := &identity.Service{Book: book, OwnerID: owner}
	runtime := identity.RandomID()
	bus := &events.Service{Book: book, Identity: identities, ValidateWork: conversations.ValidateWork, Runtime: runtime}
	intelligence := &work.Service{Book: book, Identity: identities, Conversations: conversations}
	intelligence.AuthorizeRecord = func(tx *ledger.Tx, record work.Record) error {
		if record.Request.Causation == "" {
			return nil
		}
		delivery, found, err := durable.Get[events.Delivery](tx, events.DeliveryKind, record.Request.Causation)
		if err != nil {
			return err
		}
		if !found {
			return identity.ErrDenied
		}
		if !record.Owner {
			if delivery.Rule.Handler != record.GrantID || delivery.Rule.Mode != "provider" || delivery.State != events.StateClaimed && delivery.State != events.StateDone {
				return identity.ErrDenied
			}
			return nil
		}
		rule, found, err := durable.Get[events.Subscription](tx, events.SubscriptionKind, delivery.Rule.ID)
		if err != nil {
			return err
		}
		if !found || !rule.Enabled || delivery.Rule.Mode != "work" || delivery.Rule.Conversation != record.Request.Conversation || delivery.Rule.Project != record.Request.Project {
			return identity.ErrDenied
		}
		return nil
	}
	return &Services{Book: book, Identity: identities, Providers: &provider.Service{Book: book, Identity: identities}, Work: intelligence, Events: bus, Clients: &clientcap.Broker{Book: book, Identity: identities, Runtime: runtime}}
}

func (s *Services) Pump(ctx context.Context) error {
	failures := []error{s.Events.Pump(ctx, s.Work), s.Clients.SweepUnclaimed(ctx), s.Work.Sweep(ctx)}
	for _, phase := range []struct {
		kind string
		run  func(string) error
	}{
		{work.ReadyKind, func(id string) error { return s.Work.Dispatch(ctx, id) }},
		{clientcap.ActiveKind, func(id string) error { return s.Clients.Recover(ctx, id) }},
	} {
		ids, err := s.scan.Next(ctx, s.Book, phase.kind, 32)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, id := range ids {
			if err := phase.run(id); err != nil && !errors.Is(err, ledger.ErrConflict) && !errors.Is(err, ledger.ErrStale) {
				failures = append(failures, err)
			}
		}
	}
	failures = append(failures, s.maintain(ctx))
	return errors.Join(failures...)
}

func (s *Services) Run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if err := s.Pump(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("service delivery deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
