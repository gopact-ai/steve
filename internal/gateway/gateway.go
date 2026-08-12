// Package gateway adapts Lark messages to the transport-neutral turn coordinator.
package gateway

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/turn"
)

type replier interface {
	Reply(context.Context, string, string) error
}

type processor interface {
	Handle(context.Context, string, string) (turn.Result, error)
}

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
	g.mu.Lock()
	if _, duplicate := g.seen[msg.MessageID]; msg.MessageID != "" && duplicate {
		g.mu.Unlock()
		return
	}
	if strings.TrimSpace(msg.Text) == "/cancel" {
		g.rememberLocked(msg.MessageID)
		g.mu.Unlock()
		go g.process(msg)
		return
	}
	conversationID := msg.ConversationID
	if conversationID == "" {
		conversationID = msg.ChatID
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
	result, err := g.processor.Handle(context.Background(), conversationID, msg.Text)
	if err != nil {
		log.Printf("gateway: turn failed: chat=%s error=%v", msg.ChatID, err)
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
	g.reply(msg.MessageID, out)
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
