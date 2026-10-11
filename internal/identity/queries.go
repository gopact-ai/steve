package identity

import (
	"context"
	"github.com/gopact-ai/steve/internal/durable"
)

func (s *Service) Connections(ctx context.Context, after string) (durable.Page[Grant], error) {
	return durable.PageOf[Grant](ctx, s.Book, GrantKind, after)
}
