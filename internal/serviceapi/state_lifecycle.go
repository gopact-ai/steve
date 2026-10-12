package serviceapi

import (
	"context"
	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
)

type EventLifecycle interface {
	Reconcile(context.Context, string, events.Reconciliation) error
}
type ClientLifecycle interface {
	Cancel(context.Context, string, clientcap.Decision) error
	Reconcile(context.Context, string, clientcap.Reconciliation) error
}
type Maintenance interface {
	Stats(context.Context) (durable.ServiceStats, error)
	Compact(context.Context) (durable.Compaction, error)
}
