// Package gateway adapts Lark messages to the transport-neutral turn coordinator.
package gateway

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/turn"
)

type replier interface {
	Reply(context.Context, string, string) error
}

type reactor interface {
	AddReaction(context.Context, string, string) (string, error)
	RemoveReaction(context.Context, string, string) error
}

type processor interface {
	Handle(context.Context, turn.Request) (turn.Result, error)
}

const thinkingEmoji = "THINKING"

type Gateway struct {
	processor processor
	ch        replier

	mu    sync.Mutex
	chats map[string]chan feishu.InboundMessage
	seen  map[string]struct{}
	order []string
}

func New(processor processor) *Gateway {
	return &Gateway{processor: processor, chats: map[string]chan feishu.InboundMessage{}, seen: map[string]struct{}{}}
}

func (g *Gateway) BindChannel(ch replier) { g.ch = ch }

func (g *Gateway) HandleMessage(msg feishu.InboundMessage) {
	conversationID := msg.ConversationID
	if conversationID == "" {
		conversationID = msg.ChatID
	}
	g.mu.Lock()
	if _, duplicate := g.seen[msg.MessageID]; msg.MessageID != "" && duplicate {
		g.mu.Unlock()
		return
	}
	if strings.TrimSpace(msg.Text) == "/cancel" {
		// /cancel means "stop everything in this chat": drop queued prompts
		// so they cannot run after the cancel, then cancel the in-flight
		// turn out-of-band.
		g.drainLocked(conversationID)
		g.rememberLocked(msg.MessageID)
		g.mu.Unlock()
		go g.process(msg)
		return
	}
	queue := g.chats[conversationID]
	if queue == nil {
		// ponytail: workers live for the process lifetime; add idle eviction if chat count becomes material.
		queue = make(chan feishu.InboundMessage, 16)
		g.chats[conversationID] = queue
		go g.run(queue)
	}
	select {
	case queue <- msg:
		g.rememberLocked(msg.MessageID)
		g.mu.Unlock()
	default:
		g.mu.Unlock()
		g.reply(msg.MessageID, "当前会话排队消息过多，请稍后再发。")
	}
}

// drainLocked drops every queued message of a conversation. Queued prompts
// are stale once the user cancels; their IDs are remembered so a Feishu
// redelivery does not rerun them. Requires g.mu to be held.
func (g *Gateway) drainLocked(conversationID string) {
	queue := g.chats[conversationID]
	if queue == nil {
		return
	}
	for {
		select {
		case msg := <-queue:
			g.rememberLocked(msg.MessageID)
		default:
			return
		}
	}
}

func (g *Gateway) rememberLocked(messageID string) {
	if messageID == "" {
		return
	}
	g.seen[messageID] = struct{}{}
	g.order = append(g.order, messageID)
	if len(g.order) > 4096 {
		delete(g.seen, g.order[0])
		g.order = g.order[1:]
	}
}

func (g *Gateway) run(queue <-chan feishu.InboundMessage) {
	for msg := range queue {
		g.process(msg)
	}
}

func (g *Gateway) process(msg feishu.InboundMessage) {
	conversationID := msg.ConversationID
	if conversationID == "" {
		conversationID = msg.ChatID
	}
	reactionID := g.ack(msg.MessageID)
	defer g.unack(msg.MessageID, reactionID)
	result, err := g.processor.Handle(context.Background(), turn.Request{
		ConversationID: conversationID,
		Input:          msg.Text,
		SenderOpenID:   msg.SenderOpenID,
		ChatType:       msg.ChatType,
	})
	if err != nil {
		log.Printf("gateway: turn failed: chat=%s error=%v", msg.ChatID, err)
		if errors.Is(err, context.Canceled) {
			g.reply(msg.MessageID, "任务已取消")
			return
		}
		var userErr turn.UserError
		if errors.As(err, &userErr) {
			g.reply(msg.MessageID, userErr.Text)
			return
		}
		g.reply(msg.MessageID, "Agent 调用失败，请检查 Steve 日志。")
		return
	}
	out := result.Text
	if out == "" {
		out = "(agent 本轮没有文本输出)"
	}
	if len(result.Activity) > 0 {
		out += "\n\n---\n" + strings.Join(result.Activity, "\n")
	}
	g.reply(msg.MessageID, truncateRunes(out, maxReplyRunes))
}

// maxReplyRunes keeps replies under the Feishu text message size limit so a
// long agent dump is truncated instead of silently lost.
const maxReplyRunes = 30000

func truncateRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "\n\n…(内容过长，已截断)"
}

func (g *Gateway) reply(messageID, text string) {
	if g.ch == nil {
		log.Printf("gateway: no channel bound, dropping reply")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := g.ch.Reply(ctx, messageID, text); err != nil {
		log.Printf("gateway: reply failed: %v", err)
	}
}

func (g *Gateway) ack(messageID string) string {
	r, ok := g.ch.(reactor)
	if !ok || messageID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := r.AddReaction(ctx, messageID, thinkingEmoji)
	if err != nil {
		log.Printf("gateway: ack reaction failed: %v", err)
		return ""
	}
	return id
}

func (g *Gateway) unack(messageID, reactionID string) {
	if reactionID == "" {
		return
	}
	r, ok := g.ch.(reactor)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.RemoveReaction(ctx, messageID, reactionID); err != nil {
		log.Printf("gateway: clear reaction failed: %v", err)
	}
}
