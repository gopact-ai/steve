// Package gateway routes Feishu chats onto ACP sessions with per-chat
// serialization, so one chat maps to one agent conversation.
package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"

	"acpgw/internal/acphost"
	"acpgw/internal/channel/feishu"
)

type Config struct {
	PromptTimeout time.Duration
}

type Gateway struct {
	cfg  Config
	host *acphost.Host
	ch   *feishu.Channel

	mu    sync.Mutex
	chats map[string]*chatWorker
}

type chatWorker struct {
	queue     chan feishu.InboundMessage
	sessionID acp.SessionID
}

func New(cfg Config, host *acphost.Host) *Gateway {
	return &Gateway{cfg: cfg, host: host, chats: map[string]*chatWorker{}}
}

// BindChannel gives the gateway its reply surface.
func (g *Gateway) BindChannel(ch *feishu.Channel) { g.ch = ch }

// HandleMessage enqueues one inbound message onto its chat's worker.
func (g *Gateway) HandleMessage(msg feishu.InboundMessage) {
	g.mu.Lock()
	w, ok := g.chats[msg.ChatID]
	if !ok {
		w = &chatWorker{queue: make(chan feishu.InboundMessage, 16)}
		g.chats[msg.ChatID] = w
		go g.runWorker(msg.ChatID, w)
	}
	g.mu.Unlock()

	select {
	case w.queue <- msg:
	default:
		g.reply(msg.MessageID, "⚠️ 当前会话排队消息过多，请稍后再发。")
	}
}

func (g *Gateway) runWorker(chatID string, w *chatWorker) {
	for msg := range w.queue {
		g.process(chatID, w, msg)
	}
}

func (g *Gateway) process(chatID string, w *chatWorker, msg feishu.InboundMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gateway: panic in chat %s: %v", chatID, r)
			g.reply(msg.MessageID, fmt.Sprintf("💥 内部错误: %v", r))
		}
	}()

	switch strings.TrimSpace(msg.Text) {
	case "/new", "/clear":
		w.sessionID = ""
		g.reply(msg.MessageID, "🆕 已重置会话，下一条消息将开启新对话。")
		return
	case "/status":
		state := "无活跃会话"
		if w.sessionID != "" {
			state = fmt.Sprintf("会话 %s", w.sessionID)
		}
		g.reply(msg.MessageID, fmt.Sprintf("ℹ️ %s", state))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.PromptTimeout)
	defer cancel()

	out, activity, err := g.promptWithRetry(ctx, w, msg.Text)
	if err != nil {
		log.Printf("gateway: prompt failed for chat %s: %v", chatID, err)
		g.reply(msg.MessageID, fmt.Sprintf("❌ Agent 调用失败: %v", err))
		return
	}
	if out == "" {
		out = "(agent 本轮没有文本输出)"
	}
	if len(activity) > 0 {
		out = out + "\n\n---\n" + strings.Join(activity, "\n")
	}
	g.reply(msg.MessageID, out)
}

// promptWithRetry recreates the session once if the previous one died with
// the agent process.
func (g *Gateway) promptWithRetry(ctx context.Context, w *chatWorker, text string) (string, []string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if w.sessionID == "" {
			sid, err := g.host.NewChatSession(ctx)
			if err != nil {
				return "", nil, err
			}
			w.sessionID = sid
		}
		out, activity, err := g.host.Prompt(ctx, w.sessionID, text, nil)
		if err == nil {
			return out, activity, nil
		}
		if ctx.Err() != nil || attempt == 1 {
			return out, activity, err
		}
		log.Printf("gateway: session %s failed (%v), recreating", w.sessionID, err)
		w.sessionID = ""
	}
	return "", nil, fmt.Errorf("unreachable")
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
