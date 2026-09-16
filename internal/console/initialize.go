package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type conversationInitializer interface {
	InitializeConversation(ctx context.Context, conversation, project, requester string) error
}

var _ consoleapi.ConversationInitializer = (*Service)(nil)

// InitializeConversation binds the project and persists an empty conversation
// without sending a command or admitting any work.
func (s *Service) InitializeConversation(ctx context.Context, conversation, project string) error {
	conversation, project = strings.TrimSpace(conversation), strings.TrimSpace(project)
	if strings.TrimSpace(strings.TrimPrefix(conversation, Prefix)) == "" || project == "" {
		return errors.New("conversation and project are required")
	}
	if s.owner == "" {
		return errors.New("the console needs feishu.owner_open_id: it acts as the owner")
	}
	initializer, ok := s.handler.(conversationInitializer)
	if !ok {
		return errors.New("conversation initialization is not available")
	}
	conversation = ConversationID(conversation)
	s.mu.Lock()
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		s.mu.Unlock()
		return consoleapi.ErrConsoleClosing
	}
	if s.sealed[conversation] {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s is being deleted", consoleapi.ErrBusy, conversation)
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := initializer.InitializeConversation(ctx, conversation, project, s.owner); err != nil {
		s.mu.Unlock()
		return err
	}
	if _, exists := s.replies[conversation]; exists {
		s.mu.Unlock()
		return nil
	}
	previous, hadMeta := s.meta[conversation]
	m := previous
	m.UpdatedAt = time.Now().UTC()
	s.replies[conversation] = []consoleapi.Reply{}
	s.meta[conversation] = m
	if err := s.save(); err != nil {
		delete(s.replies, conversation)
		if hadMeta {
			s.meta[conversation] = previous
		} else {
			delete(s.meta, conversation)
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if s.model != nil {
		s.model.Publish(readmodel.Event{At: m.UpdatedAt, Kind: "console.meta", Conversation: conversation, Text: m.Title})
	}
	return nil
}
