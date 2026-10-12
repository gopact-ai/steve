package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	ids["consumer"] = consumer.Grant.ID
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

// Explicitly opted-in visual preview. Both sides use the real durable bus;
// the default regression fixture never runs this producer/consumer loop.
func previewBusMotion(ctx context.Context, services *servicehub.Services, producer, consumer string) {
	publisher := identity.Principal{GrantID: producer}
	reader := identity.Principal{GrantID: consumer}
	wait := func(delay time.Duration) bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	start := time.Now()
	lastHeartbeat := start
	go func() {
		for sequence := 0; ctx.Err() == nil; sequence++ {
			_, err := services.Events.Publish(ctx, publisher, events.Publish{Key: fmt.Sprintf("motion/%d", sequence), Type: "fixture.notice", Subject: "Isolated bus animation preview", Payload: json.RawMessage(`{"preview":true}`)})
			delay := 100 * time.Millisecond
			if time.Since(start)%(32*time.Second) >= 16*time.Second {
				delay = 240 * time.Millisecond
			}
			if err != nil || !wait(delay) {
				return
			}
		}
	}()
	for ctx.Err() == nil {
		if time.Since(lastHeartbeat) >= 15*time.Second {
			if err := services.Providers.Heartbeat(ctx, publisher, 1); err != nil {
				return
			}
			if err := services.Providers.Heartbeat(ctx, reader, 1); err != nil {
				return
			}
			lastHeartbeat = time.Now()
		}
		claim, found, err := services.Events.Poll(ctx, reader)
		if err != nil {
			return
		}
		if found {
			if err := services.Events.Ack(ctx, reader, claim.Delivery.ID, events.Acknowledgement{Token: claim.Delivery.Token, State: events.StateDone, Receipt: "preview:" + claim.Delivery.ID}); err != nil {
				return
			}
		}
		delay := 200 * time.Millisecond
		if time.Since(start)%(32*time.Second) >= 16*time.Second {
			delay = 80 * time.Millisecond
		}
		if !wait(delay) {
			return
		}
	}
}
