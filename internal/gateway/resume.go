package gateway

import (
	"context"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
)

// Revival is one interrupted task the gateway picks back up after a restart.
type Revival struct {
	TaskID         string
	Goal           string
	Member         string
	ConversationID string
	ChatID         string
	MessageID      string
	Requester      string
	ChatType       string
}

type textReplier interface {
	ReplyText(ctx context.Context, messageID, text string) (string, error)
}

// Revive continues tasks a dead gateway left mid-turn. Each revival first
// clears the crash taint on its session, then posts a visible notice as a
// reply to the task's last anchor message — the notice becomes the new
// anchor, so the resumed turn renders its card in the right conversation
// and topic — and finally re-enters the normal message path with a
// continuation prompt addressed to the task's member. The agent reloads its
// own history on session load, so "continue" means exactly that.
func (g *Gateway) Revive(revivals []Revival, revive func(conversationID, member string) error) {
	tr, ok := g.ch.(textReplier)
	if !ok {
		log.Printf("gateway: channel cannot post resume notices; %d tasks stay stopped", len(revivals))
		return
	}
	for _, r := range revivals {
		if r.ConversationID == "" || r.MessageID == "" || r.Member == "" {
			log.Printf("gateway: task #%s not resumable: incomplete anchor", r.TaskID)
			continue
		}
		if err := revive(r.ConversationID, r.Member); err != nil {
			log.Printf("gateway: revive session for task #%s: %v", r.TaskID, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		noticeID, err := tr.ReplyText(ctx, r.MessageID, g.text.T(i18n.ResumeNotice, r.TaskID))
		cancel()
		if err != nil || noticeID == "" {
			log.Printf("gateway: post resume notice for task #%s: %v", r.TaskID, err)
			continue
		}
		log.Printf("gateway: resuming task #%s conversation=%s member=%s", r.TaskID, r.ConversationID, r.Member)
		g.HandleMessage(feishu.InboundMessage{
			ConversationID: r.ConversationID,
			ChatID:         r.ChatID,
			MessageID:      noticeID,
			SenderOpenID:   r.Requester,
			ChatType:       protocol.ParseChatType(r.ChatType),
			Mentioned:      true,
			Text:           "@" + r.Member + " " + g.text.T(i18n.ResumePrompt, r.Goal),
		})
	}
}
