package durable

import (
	"context"
	"github.com/gopact-ai/steve/internal/ledger"
	"strings"
	"sync"
)

// Scanner rotates bounded queue pages, so long-running low-key records cannot
// starve later work. Cursors are hints only; durable state decides admission.
type Scanner struct {
	mu      sync.Mutex
	cursors map[string]string
}

// NextStates covers legacy records before new pending indexes existed. The
// bounded cursor rotates independently of history and never returns cold data.
func (s *Scanner) NextStates(ctx context.Context, book *ledger.Ledger, kind string, states []string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[string]string{}
	}
	key := kind + ":" + strings.Join(states, ",")
	var ids []string
	err := book.Read(ctx, func(tx *ledger.ReadTx) error {
		marks := make([]string, len(states))
		args := []any{kind, s.cursors[key]}
		for i, state := range states {
			marks[i] = "?"
			args = append(args, state)
		}
		args = append(args, limit+1)
		rows, err := tx.Query(`SELECT id FROM bindings WHERE kind=? AND id>? AND json_extract(data,'$.state') IN (`+strings.Join(marks, ",")+`) ORDER BY id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		last := ""
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if len(ids) == limit {
				s.cursors[key] = last
				return nil
			}
			ids = append(ids, id)
			last = id
		}
		s.cursors[key] = ""
		return rows.Err()
	})
	return ids, err
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
