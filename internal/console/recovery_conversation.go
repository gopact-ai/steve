package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// RecoveryConversation is a display association with an existing external
// parent task. It does not alter task, execution or native question identities.
type RecoveryConversation struct {
	ParentTaskID       string
	SourceChannel      string
	SourceConversation string
	Project            string
}

func (s *Service) EnsureRecoveryConversation(ctx context.Context, source RecoveryConversation) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, value := range []string{source.ParentTaskID, source.SourceChannel, source.SourceConversation, source.Project} {
		if strings.TrimSpace(value) == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return "", consoleapi.ErrInvalidAnswer
		}
	}
	if s.owner == "" {
		return "", consoleapi.ErrQuestionForbidden
	}
	conversation := Prefix + "recovery:" + url.PathEscape(source.ParentTaskID)
	channelName := source.SourceChannel
	if source.SourceChannel == "feishu" || source.SourceChannel == "lark" {
		channelName = "飞书"
	}
	title := clipTitle(fmt.Sprintf("%s · 任务 #%s", channelName, source.ParentTaskID))
	body := fmt.Sprintf("来源：%s\n原会话：%s\n原父任务：#%s\n项目：%s\n\n原任务的待答问题将在此显示。", source.SourceChannel, source.SourceConversation, source.ParentTaskID, source.Project)
	hash := sha256.Sum256([]byte(source.ParentTaskID))
	noticeID := "recovery-source-" + hex.EncodeToString(hash[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.recoveryStoppedLocked() {
		return "", consoleapi.ErrConsoleClosing
	}
	if previous, exists := s.replies[conversation]; exists {
		for _, reply := range previous {
			if reply.ID == noticeID {
				if reply.Kind != "notice" || reply.ProjectID != source.Project || reply.Text != body || reply.Format != "text" {
					return "", consoleapi.ErrQuestionConflict
				}
				return conversation, nil
			}
		}
		return "", consoleapi.ErrQuestionConflict
	}
	if _, exists := s.meta[conversation]; exists {
		return "", consoleapi.ErrQuestionConflict
	}
	now := time.Now().UTC()
	s.meta[conversation] = Meta{Title: title, TitleBy: "system", UpdatedAt: now}
	notice := s.recordLocked(consoleapi.Reply{ID: noticeID, At: now, Conversation: conversation, ProjectID: source.Project, Title: title, Text: body, Format: "text", Kind: "notice"})
	if err := s.save(); err != nil {
		delete(s.meta, conversation)
		delete(s.replies, conversation)
		return "", err
	}
	s.publishReply(notice)
	if s.model != nil {
		s.model.Publish(readmodel.Event{At: now, Kind: "console.meta", Conversation: conversation, Text: title})
	}
	return conversation, nil
}
