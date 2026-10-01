package console

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
)

func (e *queuedExchange) recoveryReply() *consoleapi.Reply {
	if e.RecoveryAbandon != nil {
		return e.RecoveryAbandon
	}
	return e.RecoveryStop
}

// The receipt describes the owner's abandonment, never native stop evidence.
// Saving it before interrupting its waiter makes delivery restartable.
func (s *Service) recordAbandonment(target *queuedExchange, id string) error {
	s.mu.Lock()
	if target.State.Terminal() {
		s.mu.Unlock()
		return nil
	}
	if target.recoveryStopping != nil {
		s.mu.Unlock()
		return errors.New("original recovery is still processing its previous stop")
	}
	previous := target.RecoveryAbandon
	text := i18n.New(i18n.FromLang(target.Locale))
	target.RecoveryAbandon = &consoleapi.Reply{AttemptID: id, Text: text.T(i18n.ConsoleAbandoned)}
	if err := s.save(); err != nil {
		target.RecoveryAbandon = previous
		s.mu.Unlock()
		return err
	}
	cancel := target.cancel
	if cancel == nil {
		select {
		case <-target.done:
			target.done = make(chan struct{})
			s.running[target.Conversation]++
		default:
		}
	}
	reply := *target.RecoveryAbandon
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	} else {
		s.finish(target, reply, nil)
	}
	return nil
}

func (s *Service) deliverAbandonment(r attempt.Record) error {
	if r.Abandoned == nil || r.Abandoned.ProjectedAt.IsZero() {
		return errors.New("abandonment session projection is not complete")
	}
	if !strings.HasPrefix(r.Abandoned.MessageID, AnchorMark) {
		return nil
	}
	id := strings.TrimPrefix(r.Abandoned.MessageID, AnchorMark)
	s.mu.Lock()
	var target *queuedExchange
	for _, e := range s.exchanges[r.Abandoned.Conversation] {
		if e.ID == id {
			target = e
			break
		}
	}
	s.mu.Unlock()
	if target == nil {
		return nil
	}
	return s.recordAbandonment(target, r.ID)
}

func (s *Service) finishAbandonedRecovery(ctx context.Context, target *queuedExchange, id string, release func()) error {
	release()
	return s.recordAbandonment(target, id)
}
