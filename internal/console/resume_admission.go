package console

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// DispatchResume wakes only the accepted input's conversation. It does not
// release startup recovery's accounting barrier, even in another conversation.
func (s *Service) DispatchResume(conversation string, admission task.ResumeAdmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.continuationLocked(conversation, admission.ID)
	if e == nil || e.ResumeAdmission != admission {
		return fmt.Errorf("resume input %s is not accepted", admission.ID)
	}
	if e.State != consoleapi.ExchangeQueued {
		return nil
	}
	return s.startNextLocked(e.Conversation)
}

func (s *Service) checkResumeLocked(e *queuedExchange) error {
	if s.book == nil {
		return errors.New("manual resume requires durable task authority")
	}
	if e.ExpectedTask != e.ResumeAdmission.TaskID {
		return task.ErrExecutionStopped
	}
	return s.book.Read(context.Background(), func(tx *ledger.ReadTx) error {
		return task.CheckResumeAdmissionTx(tx, e.ResumeAdmission)
	})
}

// A rejected reference is a channel receipt, not a change to task authority.
// Keep its stable key so retry can never build another input under that key.
func (s *Service) rejectResumeLocked(e *queuedExchange, cause error) error {
	previous := *e
	previousReplies := s.replies[e.Conversation]
	reply := consoleapi.Reply{Conversation: e.Conversation, ExchangeID: e.ID, Kind: "reply", Text: i18n.New(i18n.FromLang(e.Locale)).T(i18n.ConsoleResumeRefused, e.ExpectedTask), Error: cause.Error(), At: time.Now().UTC()}
	reply = s.recordLocked(reply)
	e.State, e.Receipt = consoleapi.ExchangeFailed, &reply
	if err := s.save(); err != nil {
		*e = previous
		s.replies[e.Conversation] = previousReplies
		return err
	}
	e.outcome = replyOutcome(reply)
	close(e.done)
	s.publishReply(reply)
	s.publishQueue(e.Conversation)
	return nil
}
