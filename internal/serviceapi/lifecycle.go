package serviceapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/provider"
)

type ConnectionLifecycle interface {
	UpdateScope(context.Context, string, identity.ScopeUpdate) (identity.Grant, error)
	RotateCredential(context.Context, string, identity.RotateRequest) (identity.Enrollment, error)
	Audit(context.Context, string) (durable.Page[identity.ConnectionChange], error)
}

type Providers interface {
	Register(context.Context, identity.Principal, provider.Register) (provider.Registration, error)
	Heartbeat(context.Context, identity.Principal, uint64) error
	Registrations(context.Context, string) (durable.Page[provider.Registration], error)
	SelfRegistration(context.Context, identity.Principal) (provider.Registration, error)
}
