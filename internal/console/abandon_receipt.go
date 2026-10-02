package console

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
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
	defer s.mu.Unlock()
	if target.State.Terminal() {
		return nil
	}
	if s.recoveryStoppedLocked() {
		return consoleapi.ErrConsoleClosing
	}
	if target.recoveryStopping != nil {
		return errors.New("original recovery is still processing its previous stop")
	}
	text := i18n.New(i18n.FromLang(target.Locale))
	return s.finishAbandonmentLocked(target, consoleapi.Reply{AttemptID: id, Text: text.T(i18n.ConsoleAbandoned)})
}

func (s *Service) deliverAbandonment(ctx context.Context, r attempt.Record) error {
	if r.Abandoned == nil || r.Abandoned.ProjectedAt.IsZero() {
		return errors.New("abandonment session projection is not complete")
	}
	if r.Kind != attempt.KindChat && r.Kind != attempt.KindPlan {
		return nil
	}
	if err := s.checkAbandonmentInput(ctx, r); err != nil {
		return err
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

// The durable decision supplies the destination; delivery checks that it still
// describes the original input instead of borrowing the task's current anchor.
func (s *Service) checkAbandonmentInput(ctx context.Context, r attempt.Record) error {
	if r.Kind == attempt.KindChat {
		if r.TurnID == "" || r.Abandoned.MessageID != r.TurnID {
			return attempt.ErrAbandonInput
		}
		return nil
	}
	s.mu.Lock()
	book := s.book
	s.mu.Unlock()
	if book == nil {
		return attempt.ErrAbandonInput
	}
	return book.Read(ctx, func(tx *ledger.ReadTx) error {
		original, err := attempt.AbandonInputTx(tx, r)
		if err != nil {
			return err
		}
		tracked, found, err := task.GetTx(tx, r.TaskID)
		if err != nil {
			return err
		}
		if !found || tracked.Channel != r.Abandoned.Conversation || original != r.Abandoned.MessageID {
			return attempt.ErrAbandonInput
		}
		return nil
	})
}
