package durable

import (
	"context"
	"github.com/gopact-ai/steve/internal/ledger"
	"sync"
)

// Scanner rotates bounded queue pages, so long-running low-key records cannot
// starve later work. Cursors are hints only; durable state decides admission.
type Scanner struct {
	mu      sync.Mutex
	cursors map[string]string
}

func (s *Scanner) Next(ctx context.Context, book *ledger.Ledger, kind string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[string]string{}
	}
	ids, next, err := List[string](ctx, book, kind, s.cursors[kind], limit)
	if err != nil {
		return nil, err
	}
	s.cursors[kind] = next
	return ids, nil
}
