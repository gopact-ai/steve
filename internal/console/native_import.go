package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
)

func NativeImportConversation(command string) (string, error) {
	if command == "" || len(command) > 512 {
		return "", errors.New("native import requires an idempotency command")
	}
	hash := sha256.Sum256([]byte(command))
	return Prefix + "import:" + hex.EncodeToString(hash[:]), nil
}

// ImportedConversation recovers a completed import without contacting a source
// machine that may since have been disconnected or had its history archived.
func (s *Service) ImportedConversation(node string, req consoleapi.NativeImportRequest) (consoleapi.ImportedSession, bool, error) {
	conversation, err := NativeImportConversation(req.CommandID)
	if err != nil {
		return consoleapi.ImportedSession{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.meta[conversation]
	if !ok {
		return consoleapi.ImportedSession{}, false, nil
	}
	previous := meta.NativeImport
	if previous == nil || previous.Node != node || previous.Agent != req.Agent || (req.Project != "" && previous.Project != req.Project) || previous.Reference.Harness != req.Source.Harness || (req.Source.Home != "" && previous.Reference.SourceHome != req.Source.Home) || previous.Reference.NativeID != req.NativeID || previous.Reference.Revision != req.Revision {
		return consoleapi.ImportedSession{}, false, consoleapi.ErrQuestionConflict
	}
	return *previous, true, nil
}

// EnsureImportedConversation keeps the import receipt in durable metadata,
// independent of transcript pruning. Binding completes before it becomes visible.
func (s *Service) EnsureImportedConversation(ctx context.Context, command string, origin consoleapi.ImportedSession, bind func(string) error) (consoleapi.ImportedSession, error) {
	if command == "" || len(command) > 512 || s.owner == "" || bind == nil {
		return origin, errors.New("native import requires an owner, command and session binding")
	}
	hash := sha256.Sum256([]byte(command))
	conversation, _ := NativeImportConversation(command)
	origin.Conversation = conversation
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.recoveryStoppedLocked() || ctx.Err() != nil {
		return origin, consoleapi.ErrConsoleClosing
	}
	if previous, exists := s.meta[conversation]; exists {
		if previous.NativeImport != nil && *previous.NativeImport == origin {
			return origin, nil
		}
		return origin, consoleapi.ErrQuestionConflict
	}
	if len(s.replies[conversation]) != 0 || len(s.exchanges[conversation]) != 0 {
		return origin, consoleapi.ErrQuestionConflict
	}
	if err := bind(conversation); err != nil {
		return origin, err
	}
	title := clipTitle(fmt.Sprintf("%s · %s", origin.Reference.Harness, origin.Reference.NativeID))
	now := time.Now().UTC()
	s.meta[conversation] = Meta{Title: title, TitleBy: "system", UpdatedAt: now, NativeImport: &origin}
	body := i18n.FromContext(ctx).T(i18n.NativeHistoryImported, origin.Reference.Harness, origin.Node, origin.Reference.NativeID, origin.Reference.SourceWorkdir)
	notice := s.recordLocked(consoleapi.Reply{ID: "native-import-" + hex.EncodeToString(hash[:]), Conversation: conversation, ProjectID: origin.Project, At: now, Title: title, Text: body, Format: "text", Kind: "notice"})
	if err := s.save(); err != nil {
		delete(s.meta, conversation)
		delete(s.replies, conversation)
		return origin, err
	}
	s.publishReply(notice)
	return origin, nil
}
