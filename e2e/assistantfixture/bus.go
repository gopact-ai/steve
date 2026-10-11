package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/provider"
	"github.com/gopact-ai/steve/internal/servicehub"
)

func seedBus(ctx context.Context, services *servicehub.Services, producer identity.Grant) (map[string]string, error) {
	consumer, err := services.Identity.Enroll(ctx, identity.Grant{Key: "bus-consumer", Name: "Mail Consumer", Kind: "provider"})
	if err != nil {
		return nil, err
	}
	principal := identity.Principal{GrantID: consumer.Grant.ID}
	if _, err := services.Providers.Register(ctx, principal, provider.Register{PluginID: "mail.review", Version: "1", Events: []provider.Contract{}}); err != nil {
		return nil, err
	}
	rule := events.Subscription{ID: "bus-review", Name: "Review event and retain receipt", Enabled: true, EventType: "fixture.notice", Mode: "provider", Handler: consumer.Grant.ID}
	if _, err := services.Events.SaveSubscription(ctx, rule); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, state := range []string{events.StateDone, events.StateUnknown, events.StatePending} {
		event, err := services.Events.Publish(ctx, identity.Principal{GrantID: producer.ID}, events.Publish{Key: "bus/" + state, Type: "fixture.notice", Subject: strings.Repeat("A long subject for narrow layout verification ", 6), Payload: json.RawMessage(`{"subject":"A recorded event for delivery inspection","body":"Only an exact consumer receipt proves this delivery completed."}`)})
		if err != nil {
			return nil, err
		}
		if err := services.Events.Route(ctx, event.ID); err != nil {
			return nil, err
		}
		ids[state+"_event"] = event.ID
		if state == events.StatePending {
			continue
		}
		claim, found, err := services.Events.Poll(ctx, principal)
		if err != nil {
			return nil, err
		}
		if !found || claim.Event.ID != event.ID {
			return nil, errors.New("fixture did not claim the original event")
		}
		ack := events.Acknowledgement{Token: claim.Delivery.Token, State: state, Receipt: "receipt:mail-review-complete"}
		if state == events.StateUnknown {
			ack.Receipt = ""
			ack.Detail = "External reply needs reconciliation"
		}
		if err := services.Events.Ack(ctx, principal, claim.Delivery.ID, ack); err != nil {
			return nil, err
		}
		ids[state] = claim.Delivery.ID
	}
	rule.Revision = 1
	rule.Name = "Updated consumer rule"
	if _, err := services.Events.SaveSubscription(ctx, rule); err != nil {
		return nil, err
	}
	return ids, nil
}
