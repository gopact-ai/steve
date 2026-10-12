package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/conversationapi"
	"github.com/gopact-ai/steve/internal/i18n"
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
			return "", conversationapi.ErrInvalidAnswer
		}
	}
	if s.owner == "" {
		return "", conversationapi.ErrQuestionForbidden
	}
	conversation := Prefix + "recovery:" + url.PathEscape(source.ParentTaskID)
	// The notice is named after the whole source, so the page is found
	// again by what it was opened for; its wording is in whichever
	// language the page was made and decides nothing.
	hash := sha256.Sum256([]byte(strings.Join([]string{source.ParentTaskID, source.SourceChannel, source.SourceConversation, source.Project}, "\x00")))
	noticeID := "recovery-source-" + hex.EncodeToString(hash[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.recoveryStoppedLocked() {
		return "", conversationapi.ErrConsoleClosing
	}
	// The page is introduced in the console's language when it is made.
	text := i18n.New(i18n.FromLang(s.submissionLocaleLocked(ctx, nil)))
	channelName := source.SourceChannel
	if source.SourceChannel == "feishu" || source.SourceChannel == "lark" {
		channelName = text.T(i18n.ConsoleRecoveryFeishu)
	}
	title := clipTitle(text.T(i18n.ConsoleRecoveryTitle, channelName, source.ParentTaskID))
	body := recoveryIntroduction(text, source)
	if previous, exists := s.replies[conversation]; exists {
		for _, reply := range previous {
			if reply.ID == noticeID {
				if reply.Kind != "notice" || reply.ProjectID != source.Project || reply.Format != "text" {
					return "", conversationapi.ErrQuestionConflict
				}
				return conversation, nil
			}
		}
		return "", conversationapi.ErrQuestionConflict
	}
	if _, exists := s.meta[conversation]; exists {
		return "", conversationapi.ErrQuestionConflict
	}
	now := time.Now().UTC()
	s.meta[conversation] = Meta{Title: title, TitleBy: "system", UpdatedAt: now}
	notice := s.recordLocked(conversationapi.Reply{ID: noticeID, At: now, Conversation: conversation, ProjectID: source.Project, Title: title, Text: body, Format: "text", Kind: "notice"})
	if err := s.save(); err != nil {
		delete(s.meta, conversation)
		delete(s.replies, conversation)
		return "", err
	}
	s.publishReply(notice)
	if s.model != nil {
		s.model.Publish(conversationapi.Event{At: now, Kind: "console.meta", Conversation: conversation, Text: title})
	}
	return conversation, nil
}

// recoveryIntroduction is the notice that opens a recovery page.
func recoveryIntroduction(text i18n.Catalog, source RecoveryConversation) string {
	return text.T(i18n.ConsoleRecoveryBody, source.SourceChannel, source.SourceConversation, source.ParentTaskID, source.Project)
}
