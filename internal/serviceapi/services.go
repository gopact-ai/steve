// Package serviceapi defines transport ports for the assistant services. A
// transport receives no database handle, transaction, harness or owner bypass.
package serviceapi

import (
	"context"
	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/work"
)

type Identity interface {
	Authenticate(context.Context, string) (identity.Principal, error)
	Enroll(context.Context, identity.Grant) (identity.Enrollment, error)
	Revoke(context.Context, string) error
	Connections(context.Context, string) (durable.Page[identity.Grant], error)
}
type Events interface {
	Publish(context.Context, identity.Principal, events.Publish) (events.Event, error)
	Poll(context.Context, identity.Principal) (events.Claimed, bool, error)
	Ack(context.Context, identity.Principal, string, events.Acknowledgement) error
	SaveSubscription(context.Context, events.Subscription) (events.Subscription, error)
	Subscriptions(context.Context, string) (durable.Page[events.Subscription], error)
	InboxEntries(context.Context, string) (durable.Page[events.Inbox], error)
	Deliveries(context.Context, string) (durable.Page[events.Delivery], error)
	Event(context.Context, string) (events.Event, error)
	ResolveInbox(context.Context, string) error
	Reprocess(context.Context, string) error
	Retry(context.Context, string) error
}
type Work interface {
	Submit(context.Context, identity.Principal, work.Request) (work.Record, error)
	Status(context.Context, identity.Principal, string) (work.Record, work.Result, error)
}
type Clients interface {
	Attention(context.Context, string) (durable.Page[clientcap.Invocation], error)
	Invoke(context.Context, identity.Principal, clientcap.Request) (clientcap.Invocation, error)
	Register(context.Context, identity.Principal, clientcap.Register) (clientcap.Registration, error)
	Heartbeat(context.Context, identity.Principal, uint64) error
	Poll(context.Context, identity.Principal, uint64) (clientcap.Invocation, bool, error)
	Complete(context.Context, identity.Principal, string, clientcap.Completion) error
	Status(context.Context, identity.Principal, string) (clientcap.Invocation, error)
	Available(context.Context, identity.Principal, string) ([]clientcap.Registration, error)
	Approve(context.Context, string, bool) error
	Registrations(context.Context, string) (durable.Page[clientcap.Registration], error)
	Invocations(context.Context, string) (durable.Page[clientcap.Invocation], error)
}
type Services struct {
	Identity Identity
	Events   Events
	Work     Work
	Clients  Clients
}
