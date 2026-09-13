package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// EnsureImportedConversation keeps the import receipt in durable metadata,
// independent of transcript pruning. Binding completes before it becomes visible.
func (s *Service) EnsureImportedConversation(ctx context.Context, command string, origin consoleapi.ImportedSession, bind func(string) error) (consoleapi.ImportedSession, error) {
	if command == "" || len(command) > 512 || s.owner == "" || bind == nil {
		return origin, errors.New("native import requires an owner, command and session binding")
	}
	hash := sha256.Sum256([]byte(command))
	conversation := Prefix + "import:" + hex.EncodeToString(hash[:])
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
	body := fmt.Sprintf("已导入历史会话\n工具：%s\n机器：%s\n原会话：%s\n目录：%s\n\n发送下一条消息后将恢复此上下文。", origin.Reference.Harness, origin.Node, origin.Reference.NativeID, origin.Reference.SourceWorkdir)
	notice := s.recordLocked(consoleapi.Reply{ID: "native-import-" + hex.EncodeToString(hash[:]), Conversation: conversation, ProjectID: origin.Project, At: now, Title: title, Text: body, Format: "text", Kind: "notice"})
	if err := s.save(); err != nil {
		delete(s.meta, conversation)
		delete(s.replies, conversation)
		return origin, err
	}
	s.publishReply(notice)
	return origin, nil
}
