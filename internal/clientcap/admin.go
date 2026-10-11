package clientcap

import (
	"context"
	"github.com/gopact-ai/steve/internal/durable"
)

func (b *Broker) Attention(ctx context.Context, after string) (durable.Page[Invocation], error) {
	page, err := durable.PageByIndex[Invocation](ctx, b.Book, AttentionKind, CallKind, after)
	for i := range page.Items {
		page.Items[i].Token = ""
	}
	return page, err
}

func (b *Broker) Registrations(ctx context.Context, after string) (durable.Page[Registration], error) {
	return durable.PageOf[Registration](ctx, b.Book, ClientKind, after)
}
func (b *Broker) Invocations(ctx context.Context, after string) (durable.Page[Invocation], error) {
	page, err := durable.PageOf[Invocation](ctx, b.Book, CallKind, after)
	for i := range page.Items {
		page.Items[i].Token = ""
	}
	return page, err
}
