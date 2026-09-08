package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
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
		slog.Error(fmt.Sprintf("gateway: notice for task #%s: %v", n.TaskID, err), "task", n.TaskID, "message", n.MessageID)
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
		slog.Warn(fmt.Sprintf("gateway: channel cannot post resume notices; %d tasks stay stopped", len(revivals)))
		return
	}
	for _, r := range revivals {
		g.ResumeTask(r, revive)
	}
}

// Deliver puts a message from the platform into a task's chat: a
// delegated child's result reaching its parent. The notice is posted as
// a reply at the task's anchor and becomes the anchor of the turn the
// prompt starts, the way a resume does.
func (g *Gateway) Deliver(r Revival, notice, prompt string) error {
	tr, ok := g.ch.(textReplier)
	if !ok {
		return fmt.Errorf("channel cannot post notices")
	}
	if r.ConversationID == "" || r.MessageID == "" || r.Member == "" {
		return fmt.Errorf("task #%s: incomplete anchor", r.TaskID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	noticeID, err := tr.ReplyText(ctx, r.MessageID, notice)
	cancel()
	if err != nil {
		return err
	}
	if noticeID == "" {
		return fmt.Errorf("task #%s: notice posted without an id", r.TaskID)
	}
	g.HandleMessage(feishu.InboundMessage{
		ConversationID: r.ConversationID,
		ChatID:         r.ChatID,
		MessageID:      noticeID,
		SenderOpenID:   r.Requester,
		ChatType:       protocol.ParseChatType(r.ChatType),
		Mentioned:      true,
		Text:           "@" + r.Member + " " + prompt,
	})
	return nil
}

// ResumeTask picks one task back up. The notice is not decoration: it is the
// new anchor. A replayed message needs an id of its own — reusing the old one
// would be dropped as a duplicate, and the resumed turn would have nothing to
// render its card against.
func (g *Gateway) ResumeTask(r Revival, revive func(conversationID, member string) error) {
	tr, ok := g.ch.(textReplier)
	if !ok {
		slog.Warn(fmt.Sprintf("gateway: channel cannot post resume notices; task #%s stays stopped", r.TaskID), "task", r.TaskID)
		return
	}
	if r.ConversationID == "" || r.MessageID == "" || r.Member == "" {
		slog.Warn(fmt.Sprintf("gateway: task #%s not resumable: incomplete anchor", r.TaskID), "task", r.TaskID)
		return
	}
	if err := revive(r.ConversationID, r.Member); err != nil {
		slog.Error(fmt.Sprintf("gateway: revive session for task #%s: %v", r.TaskID, err), "task", r.TaskID, "conversation", r.ConversationID, "member", r.Member)
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
		slog.Error(fmt.Sprintf("gateway: post resume notice for task #%s: %v", r.TaskID, err), "task", r.TaskID, "conversation", r.ConversationID, "message", r.MessageID)
		return
	}
	slog.Info(fmt.Sprintf("gateway: resuming task #%s conversation=%s member=%s manual=%t", r.TaskID, r.ConversationID, r.Member, r.Manual), "task", r.TaskID, "conversation", r.ConversationID, "member", r.Member)
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

// Fire is one scheduled run. It carries the same anchor-and-replay shape as a
// revival because it is the same problem: Steve has something to say in a
// conversation nobody is currently typing in, and the only way to say it as a
// turn is to become a message first.
type Fire struct {
	Channel        string
	ProjectID      string
	ScheduleID     string
	ConversationID string
	ChatID         string
	ChatType       string
	MessageID      string
	Requester      string
	Member         string
	Prompt         string
}

type FireReceipt struct{ MessageID string }

// FireSchedule announces the run at the schedule's anchor and then replays the
// stored instruction as a message from the person who scheduled it. The notice
// is the new anchor: a replayed message needs an id of its own, and the
// announcement is also what makes an unattended run visible rather than
// something that just appears.
func (g *Gateway) FireSchedule(ctx context.Context, f Fire) (FireReceipt, error) {
	tr, ok := g.ch.(textReplier)
	if !ok {
		return FireReceipt{}, fmt.Errorf("channel cannot post schedule notice for #%s", f.ScheduleID)
	}
	if f.Channel != "feishu" || strings.HasPrefix(f.ConversationID, "console:") || f.ChatID == "console" {
		return FireReceipt{}, fmt.Errorf("schedule %s does not target the Feishu channel", f.ScheduleID)
	}
	if f.ConversationID == "" || f.MessageID == "" || f.Prompt == "" || f.Member == "" || f.Requester == "" || f.ProjectID == "" {
		return FireReceipt{}, fmt.Errorf("schedule %s has incomplete execution context", f.ScheduleID)
	}
	if validator, ok := g.processor.(interface {
		ValidateScheduled(context.Context, string, string, string) error
	}); ok {
		if err := validator.ValidateScheduled(ctx, f.ConversationID, f.ProjectID, f.Requester); err != nil {
			return FireReceipt{}, err
		}
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	noticeID, err := tr.ReplyText(noticeCtx, f.MessageID, g.text.T(i18n.ScheduleNotice, f.ScheduleID))
	cancel()
	if err != nil {
		var network net.Error
		if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = errors.Join(channel.ErrOutcomeUnknown, err)
		}
		return FireReceipt{}, fmt.Errorf("schedule notice: %w", err)
	}
	if noticeID == "" {
		return FireReceipt{}, fmt.Errorf("%w: schedule notice has no receipt", channel.ErrOutcomeUnknown)
	}
	text := f.Prompt
	if f.Member != "" {
		text = "@" + f.Member + " " + text
	}
	slog.Info(fmt.Sprintf("gateway: firing schedule #%s conversation=%s member=%s", f.ScheduleID, f.ConversationID, f.Member), "schedule", f.ScheduleID, "conversation", f.ConversationID, "member", f.Member)
	msg := feishu.InboundMessage{
		ConversationID: f.ConversationID,
		ChatID:         f.ChatID,
		MessageID:      noticeID,
		SenderOpenID:   f.Requester,
		ChatType:       protocol.ParseChatType(f.ChatType),
		Mentioned:      true,
		Origin:         "schedule:" + f.ScheduleID,
		Text:           text,
	}
	// The firing record owns the durable dispatch. Return a receipt only
	// after Handle has actually processed this input, never after merely
	// spawning an in-memory goroutine that a restart could lose.
	g.mu.Lock()
	first := g.serving[f.ConversationID] == 0
	g.serving[f.ConversationID]++
	g.mu.Unlock()
	if first {
		g.slots <- struct{}{}
	}
	defer func() {
		g.mu.Lock()
		g.serving[f.ConversationID]--
		last := g.serving[f.ConversationID] == 0
		if last {
			delete(g.serving, f.ConversationID)
		}
		g.mu.Unlock()
		if last {
			<-g.slots
		}
	}()
	if g.gate != nil {
		g.gate.Anchor(f.ConversationID, channel.Address{Channel: "feishu", Conversation: f.ConversationID, Message: noticeID})
	}
	ui := g.newTurnUI(msg, false)
	result, runErr := g.processor.Handle(ctx, turn.Request{
		Channel: "feishu", ConversationID: f.ConversationID, Input: text, Origin: msg.Origin,
		MessageID: noticeID, ChatID: f.ChatID, CardID: ui.cardID, SenderOpenID: f.Requester,
		ChatType: protocol.ParseChatType(f.ChatType), Mentioned: true, ExpectedProject: f.ProjectID,
		OnProgress: ui.progress, OnPhase: ui.setPhase,
		OnAskUser: func(ctx context.Context, q view.Question) (view.Answer, error) { return g.askQuestion(ctx, ui, q) },
		OnAsk: func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			return g.askPermission(ctx, ui, ask)
		},
	})
	ui.finish(result, runErr)
	return FireReceipt{MessageID: noticeID}, nil
}
