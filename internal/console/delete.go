package console

import (
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// Seal closes a conversation to new work while it is being deleted, so
// nothing arrives between the check that it is idle and the moment it is
// gone. It refuses a conversation with a line still queued or running:
// deleting a thread mid-turn would leave an agent answering into nothing.
func (s *Service) Seal(conversation string) (func(), error) {
	conversation = ConversationID(conversation)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.maintenance {
		return nil, consoleapi.ErrConsoleClosing
	}
	if s.sealed[conversation] {
		return nil, fmt.Errorf("%w: %s is being deleted", consoleapi.ErrBusy, conversation)
	}
	for _, e := range s.exchanges[conversation] {
		if e.State == consoleapi.ExchangeRunning || e.State == consoleapi.ExchangeQueued {
			return nil, fmt.Errorf("%w: %s has a line %s", consoleapi.ErrBusy, conversation, e.State)
		}
	}
	if s.sealed == nil {
		s.sealed = map[string]bool{}
	}
	s.sealed[conversation] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.sealed, conversation)
	}, nil
}

// Discard forgets a conversation: its lines, its name, the exchanges it
// accepted and the questions asked in it. The caller seals it first and
// has already ended what it holds elsewhere — sessions, tasks, schedules
// — because this is the record that made those reachable.
func (s *Service) Discard(conversation string) error {
	conversation = ConversationID(conversation)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hadReplies := s.replies[conversation]
	_, hadMeta := s.meta[conversation]
	if !hadReplies && !hadMeta {
		return fmt.Errorf("no conversation %q", conversation)
	}
	for _, e := range s.exchanges[conversation] {
		if e.State == consoleapi.ExchangeRunning || e.State == consoleapi.ExchangeQueued {
			return fmt.Errorf("%w: %s has a line %s", consoleapi.ErrBusy, conversation, e.State)
		}
		delete(s.processes, e.ID)
	}
	previous := transcript{
		Replies:   map[string][]consoleapi.Reply{conversation: s.replies[conversation]},
		Meta:      map[string]Meta{conversation: s.meta[conversation]},
		Exchanges: map[string][]*queuedExchange{conversation: s.exchanges[conversation]},
		Questions: map[string]consoleapi.PendingQuestion{},
	}
	for id, q := range s.questions {
		if q.Conversation == conversation {
			previous.Questions[id] = q
			delete(s.questions, id)
		}
	}
	delete(s.replies, conversation)
	delete(s.meta, conversation)
	delete(s.exchanges, conversation)
	delete(s.running, conversation)
	if err := s.save(); err != nil {
		s.replies[conversation] = previous.Replies[conversation]
		if hadMeta {
			s.meta[conversation] = previous.Meta[conversation]
		}
		s.exchanges[conversation] = previous.Exchanges[conversation]
		for id, q := range previous.Questions {
			s.questions[id] = q
		}
		return err
	}
	for id := range previous.Questions {
		if waiter := s.questionWaiters[id]; waiter != nil {
			close(waiter)
			delete(s.questionWaiters, id)
		}
	}
	if s.model != nil {
		s.model.Publish(readmodel.Event{At: time.Now().UTC(), Kind: "console.deleted", Conversation: conversation})
	}
	return nil
}
