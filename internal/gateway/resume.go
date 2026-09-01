package gateway

import (
	"context"
	"log"
	"strings"
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
	// Leftovers of the crashed turn — the opener card and agent-sent
	// messages — recalled before the resume notice so the chat is not
	// haunted by a forever-running card and stale progress.
	OpenCard string
	Interim  []string
	// Manual marks a resume the user asked for rather than one a crash
	// forced. Nothing is recalled then: the cancelled card is an honest
	// record of the turn they stopped, not debris.
	Manual bool
}

type textReplier interface {
	ReplyText(ctx context.Context, messageID, text string) (string, error)
}

// Notice is one line Steve posts on its own initiative, outside any turn's
// card: a task ended, and saying so is the platform's job rather than the
// agent's. The mention is what makes it a delivery instead of a log entry.
type Notice struct {
	TaskID    string
	MessageID string
	Requester string
	Text      string
}

// Notify posts the notice as a reply at the task's anchor. A text message is
// deliberate: it is the second, louder knock after a card that may have
// landed in a chat nobody was watching.
func (g *Gateway) Notify(n Notice) {
	tr, ok := g.ch.(textReplier)
	if !ok || n.MessageID == "" || strings.TrimSpace(n.Text) == "" {
		return
	}
	text := n.Text
	if n.Requester != "" {
		text = "<at user_id=\"" + n.Requester + "\"></at> " + text
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := tr.ReplyText(ctx, n.MessageID, text); err != nil {
		log.Printf("gateway: notice for task #%s: %v", n.TaskID, err)
	}
}

// Revive continues tasks a dead gateway left mid-turn. Each revival first
// clears the crash taint on its session, then posts a visible notice as a
// reply to the task's last anchor message — the notice becomes the new
// anchor, so the resumed turn renders its card in the right conversation
// and topic — and finally re-enters the normal message path with a
// continuation prompt addressed to the task's member. The agent reloads its
// own history on session load, so "continue" means exactly that.
func (g *Gateway) Revive(revivals []Revival, revive func(conversationID, member string) error) {
	if _, ok := g.ch.(textReplier); !ok {
		log.Printf("gateway: channel cannot post resume notices; %d tasks stay stopped", len(revivals))
		return
	}
	for _, r := range revivals {
		g.ResumeTask(r, revive)
	}
}

// ResumeTask picks one task back up. The notice is not decoration: it is the
// new anchor. A replayed message needs an id of its own — reusing the old one
// would be dropped as a duplicate, and the resumed turn would have nothing to
// render its card against.
func (g *Gateway) ResumeTask(r Revival, revive func(conversationID, member string) error) {
	tr, ok := g.ch.(textReplier)
	if !ok {
		log.Printf("gateway: channel cannot post resume notices; task #%s stays stopped", r.TaskID)
		return
	}
	if r.ConversationID == "" || r.MessageID == "" || r.Member == "" {
		log.Printf("gateway: task #%s not resumable: incomplete anchor", r.TaskID)
		return
	}
	if err := revive(r.ConversationID, r.Member); err != nil {
		log.Printf("gateway: revive session for task #%s: %v", r.TaskID, err)
		return
	}
	if !r.Manual {
		for _, stale := range append([]string{r.OpenCard}, r.Interim...) {
			if stale != "" {
				g.recall(stale)
			}
		}
	}
	notice, prompt := i18n.ResumeNotice, i18n.ResumePrompt
	if r.Manual {
		notice, prompt = i18n.TaskResumeNotice, i18n.TaskResumeManual
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	noticeID, err := tr.ReplyText(ctx, r.MessageID, g.text.T(notice, r.TaskID))
	cancel()
	if err != nil || noticeID == "" {
		log.Printf("gateway: post resume notice for task #%s: %v", r.TaskID, err)
		return
	}
	log.Printf("gateway: resuming task #%s conversation=%s member=%s manual=%t", r.TaskID, r.ConversationID, r.Member, r.Manual)
	g.HandleMessage(feishu.InboundMessage{
		ConversationID: r.ConversationID,
		ChatID:         r.ChatID,
		MessageID:      noticeID,
		SenderOpenID:   r.Requester,
		ChatType:       protocol.ParseChatType(r.ChatType),
		Mentioned:      true,
		Text:           "@" + r.Member + " " + g.text.T(prompt, r.Goal),
	})
}
