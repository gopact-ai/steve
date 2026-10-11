package events

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
)

func (s *Service) Subscriptions(ctx context.Context, after string) (durable.Page[Subscription], error) {
	return durable.PageOf[Subscription](ctx, s.Book, SubscriptionKind, after)
}
func (s *Service) InboxEntries(ctx context.Context, after string) (durable.Page[Inbox], error) {
	return durable.PageByIndex[Inbox](ctx, s.Book, InboxOpenKind, InboxKind, after)
}
func (s *Service) Deliveries(ctx context.Context, after string) (durable.Page[Delivery], error) {
	return durable.PageOf[Delivery](ctx, s.Book, DeliveryKind, after)
}
func (s *Service) Delivery(ctx context.Context, id string) (Delivery, error) {
	out, found, err := durable.Read[Delivery](ctx, s.Book, DeliveryKind, id)
	if err == nil && !found {
		err = errors.New("delivery not found")
	}
	return out, err
}
func (s *Service) Event(ctx context.Context, id string) (Event, error) {
	out, found, err := durable.Read[Event](ctx, s.Book, EventKind, id)
	if err == nil && !found {
		err = errors.New("event not found")
	}
	return out, err
}
