package console

import (
	"context"
	"log/slog"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// refreshStopTasks serializes observations without holding the console lock
// across storage I/O. The queue only uses the resulting in-memory flags;
// durable completion guards still read task authority in their own transaction.
func (s *Service) refreshStopTasks(ctx context.Context) {
	s.stopTasksMu.Lock()
	defer s.stopTasksMu.Unlock()
	s.mu.Lock()
	book := s.book
	pending := map[*queuedExchange]string{}
	for _, list := range s.exchanges {
		for _, e := range list {
			if !e.State.Terminal() && e.RecoveryStopPending != "" && e.RecoveryStopTask != "" {
				pending[e] = e.RecoveryStopTask
			}
		}
	}
	s.mu.Unlock()
	if book == nil || len(pending) == 0 {
		return
	}
	states := map[string]bool{}
	err := book.Read(ctx, func(tx *ledger.ReadTx) error {
		for _, id := range pending {
			tracked, found, err := task.GetTx(tx, id)
			if err != nil {
				return err
			}
			states[id] = found && setAside(tracked.State)
		}
		return nil
	})
	if err != nil {
		slog.Error("console: read tasks of pending stops", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for e, id := range pending {
		if e.RecoveryStopTask == id {
			e.stopSetAside = err == nil && states[id]
		}
	}
}
