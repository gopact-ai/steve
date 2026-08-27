// Package gateway adapts Lark messages to the transport-neutral turn coordinator.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type pendingAsk struct {
	openID string
	cardID string
	done   chan string
}

// liveTurn lets a card button act on the turn that rendered it: stop while it
// runs, retry the original message after it failed.
type liveTurn struct {
	msg     feishu.InboundMessage
	cardID  string
	running bool
}

type recaller interface {
	DeleteMessage(context.Context, string) error
}

// maxLiveTurns bounds the retry history; turns are dropped oldest first.
const maxLiveTurns = 64

// approvalTimeout caps how long a turn waits for a human to tap the card.
const approvalTimeout = 3 * time.Minute

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

// agentAnchor is the messaging MCP server's view of the gateway: each
// inbound message tells it where that conversation's interim agent sends
// should attach, and starts a fresh recall epoch.
type agentAnchor interface {
	Anchor(conversationID, chatID, messageID string)
	// SetStyle hands the messaging server the turn's card tail ("codex ·
	// GPT 5.6 Sol · Agent") so interim cards read as the same family as
	// the final card.
	SetStyle(conversationID, style string)
	// Interim reports whether agent-sent cards landed in the conversation
	// this turn — if so the final answer must be posted below them, not
	// patched into the opening card above them.
	Interim(conversationID string) bool
}

const thinkingEmoji = "THINKING"

type Gateway struct {
	processor processor
	ch        replier
	text      i18n.Catalog
	gate      agentAnchor

	// slots bounds how many conversations are served at once. A turn spends
	// almost all of its time waiting on an agent subprocess rather than on
	// this process's CPU, so a little over one per core keeps the box
	// responsive without throttling ordinary use.
	slots chan struct{}

	mu       sync.Mutex
	seen     map[string]struct{}
	order    []string
	asks     map[string]*pendingAsk
	turns    map[string]*liveTurn
	ring     []string
	lastCard []byte
	// serving counts the messages in flight per conversation, so a second
	// one can tell that its conversation already holds a slot.
	serving map[string]int
}

// PoolSize is the ceiling on concurrently served conversations.
func PoolSize() int { return max(2, runtime.NumCPU()*3/2) }

func New(processor processor) *Gateway {
	return &Gateway{
		processor: processor,
		text:      i18n.New(i18n.LocaleZH),
		seen:      map[string]struct{}{},
		asks:      map[string]*pendingAsk{},
		turns:     map[string]*liveTurn{},
		serving:   map[string]int{},
		slots:     make(chan struct{}, PoolSize()),
	}
}

func (g *Gateway) BindChannel(ch replier) { g.ch = ch }

// SetAgentGate wires the messaging MCP server; call before Start.
func (g *Gateway) SetAgentGate(gate agentAnchor) { g.gate = gate }

func (g *Gateway) SetCatalog(cat i18n.Catalog) { g.text = cat }

// HandleMessage starts every message immediately instead of queueing it
// behind whatever the chat is already doing.
//
// A per-conversation queue was the wrong shape once a new message means
// "stop that, do this": a message that waits for the turn it is meant to
// interrupt can never interrupt it. Serialising is now the coordinator's
// job, and it does it by taking the turn away from the running prompt
// rather than by making the user wait.
func (g *Gateway) HandleMessage(msg feishu.InboundMessage) {
	g.mu.Lock()
	if _, duplicate := g.seen[msg.MessageID]; msg.MessageID != "" && duplicate {
		g.mu.Unlock()
		return
	}
	g.rememberLocked(msg.MessageID)
	g.mu.Unlock()
	go g.serve(msg, conversationID(msg))
}

// serve runs one message against the pool.
//
// The slot is per conversation, not per message, and that is the whole
// subtlety: a second message for a conversation already being served is an
// interrupt, and making it wait for a slot that the very turn it interrupts
// is holding would deadlock the two against each other — the same mistake
// the per-conversation queue made. So only the first message of a
// conversation takes a slot, and whoever finishes last gives it back.
func (g *Gateway) serve(msg feishu.InboundMessage, conversation string) {
	g.mu.Lock()
	first := g.serving[conversation] == 0
	g.serving[conversation]++
	g.mu.Unlock()
	if first {
		g.slots <- struct{}{}
	}
	defer func() {
		g.mu.Lock()
		g.serving[conversation]--
		last := g.serving[conversation] == 0
		if last {
			delete(g.serving, conversation)
		}
		g.mu.Unlock()
		if last {
			<-g.slots
		}
	}()
	g.process(msg)
}

func conversationID(msg feishu.InboundMessage) string {
	if msg.ConversationID != "" {
		return msg.ConversationID
	}
	return msg.ChatID
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

func silentListen(msg feishu.InboundMessage) bool {
	return msg.ChatType == protocol.ChatGroup && !msg.Mentioned
}

// topicSeeder is the channel capability /t rides on: reply into a fresh
// thread and report where it landed.
type topicSeeder interface {
	ReplyThread(context.Context, string, string) (string, string, error)
}

// seedTopic turns "/t <task>" into a new topic thread running that task —
// the entry point for parallel work in one chat. The anchor reply carries
// the task text so the topic's preview says what it is about; the task then
// runs as if it had been sent inside the new thread, so its card, answer and
// session all live there, isolated from the flat chat's own session.
func (g *Gateway) seedTopic(msg feishu.InboundMessage, task string) {
	seeder, ok := g.ch.(topicSeeder)
	if !ok {
		g.reply(msg.MessageID, g.text.T(i18n.TopicFailed))
		return
	}
	if strings.TrimSpace(task) == "" {
		g.reply(msg.MessageID, g.text.T(i18n.TopicNeedsTask, protocol.CommandTopic, protocol.CommandTopic))
		return
	}
	if msg.ConversationID != "" && msg.ConversationID != msg.ChatID {
		// Already inside a thread; a topic within a topic is not a thing
		// Feishu has, and the session here is already isolated.
		g.reply(msg.MessageID, g.text.T(i18n.TopicAlready))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	anchor, thread, err := seeder.ReplyThread(ctx, msg.MessageID, task)
	cancel()
	if err != nil || anchor == "" || thread == "" {
		log.Printf("gateway: seed topic failed: %v", err)
		g.reply(msg.MessageID, g.text.T(i18n.TopicFailed))
		return
	}
	seeded := msg
	seeded.MessageID = anchor
	seeded.ConversationID = thread
	seeded.Text = task
	seeded.Quote = ""
	g.process(seeded)
}

func (g *Gateway) process(msg feishu.InboundMessage) {
	conversationID := msg.ConversationID
	if conversationID == "" {
		conversationID = msg.ChatID
	}
	listen := silentListen(msg)
	if cmd, rest := protocol.ParseCommand(strings.TrimSpace(msg.Text)); cmd == protocol.CommandTopic && !listen {
		g.seedTopic(msg, rest)
		return
	}
	if g.gate != nil && msg.MessageID != "" {
		g.gate.Anchor(conversationID, msg.ChatID, msg.MessageID)
	}
	ui := g.newTurnUI(msg, listen)
	result, err := g.processor.Handle(context.Background(), turn.Request{
		ConversationID: conversationID,
		Input:          g.promptText(msg),
		MessageID:      msg.MessageID,
		ChatID:         msg.ChatID,
		SenderOpenID:   msg.SenderOpenID,
		ChatType:       protocol.ParseChatType(string(msg.ChatType)),
		Mentioned:      msg.Mentioned,
		Images:         inboundImages(msg),
		OnProgress:     ui.progress,
		OnPhase:        ui.setPhase,
		OnAskUser: func(ctx context.Context, q view.Question) (view.Answer, error) {
			return g.askQuestion(ctx, ui, q)
		},
		OnAsk: func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			return g.askPermission(ctx, ui, ask)
		},
	})
	if err != nil {
		log.Printf("gateway: turn failed: chat=%s error=%v", msg.ChatID, err)
	}
	ui.finish(result, err)
}

// maxReplyRunes keeps replies under the Feishu text message size limit so a
// long agent dump is truncated instead of silently lost.
const maxReplyRunes = 30000

func (g *Gateway) truncateRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "\n\n" + g.text.T(i18n.Truncated)
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

func (g *Gateway) promptText(msg feishu.InboundMessage) string {
	text := msg.Text
	if msg.Quote != "" {
		quoted := g.text.T(i18n.QuotedMessage, msg.Quote)
		if text == "" {
			text = quoted
		} else {
			text = quoted + "\n" + text
		}
	}
	if len(msg.ImageKeys) > 0 && len(msg.Images) == 0 {
		note := g.text.T(i18n.ImageDownloadFailed)
		if text == "" {
			return note
		}
		return text + "\n" + note
	}
	if text == "" && len(msg.Images) > 0 {
		return g.text.T(i18n.ImagePlaceholder)
	}
	return text
}

func inboundImages(msg feishu.InboundMessage) []harness.Media {
	if len(msg.Images) == 0 {
		return nil
	}
	out := make([]harness.Media, 0, len(msg.Images))
	for _, img := range msg.Images {
		if len(img.Data) == 0 {
			continue
		}
		out = append(out, harness.Media{MIME: img.MIME, Data: img.Data})
	}
	return out
}

func (g *Gateway) askPermission(ctx context.Context, ui *turnUI, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	ui.mu.Lock()
	cardID := ui.cardID
	closed := ui.closed || ui.listen || ui.fallback
	openID := ui.msg.SenderOpenID
	ui.mu.Unlock()
	if closed || cardID == "" {
		return permission.Choose(false, ask.Options), nil
	}
	id := newRequestID()
	pending := &pendingAsk{openID: openID, cardID: cardID, done: make(chan string, 1)}
	g.mu.Lock()
	g.asks[id] = pending
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.asks, id)
		g.mu.Unlock()
		ui.setApproval(nil)
	}()
	name := ask.ToolName
	if name == "" {
		name = string(ask.Kind)
	}
	ui.setApproval(&card.Approval{RequestID: id, ToolName: name, Reason: ask.Reason})
	log.Printf("gateway: approval %s pending: tool=%q kind=%s", id, name, ask.Kind)
	timer := time.NewTimer(approvalTimeout)
	defer timer.Stop()
	select {
	case decision := <-pending.done:
		return permission.Choose(decision == "allow", ask.Options), nil
	case <-timer.C:
		// Deny instead of stalling: the agent ends the turn cleanly rather
		// than dragging the whole prompt into its own timeout.
		log.Printf("gateway: approval %s timed out, denying", id)
		return permission.Choose(false, ask.Options), nil
	case <-ctx.Done():
		return permission.Choose(false, ask.Options), ctx.Err()
	}
}

func (g *Gateway) HandleCardAction(action feishu.CardAction) feishu.CardToast {
	log.Printf("gateway: card action=%q request=%s decision=%s user=%s message=%s",
		action.Action, action.RequestID, action.Decision, action.OpenID, action.MessageID)
	switch action.Action {
	case "tool_approval":
		return g.handleApprovalAction(action)
	case "elicit_answer":
		return g.handleQuestionAction(action)
	case "history_restore":
		return g.handleRecoverAction(action)
	case "turn_cancel":
		return g.handleStopAction(action)
	case "turn_retry":
		return g.handleRetryAction(action)
	default:
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalMalformed)}
	}
}

func (g *Gateway) registerTurn(msg feishu.InboundMessage) string {
	id := newRequestID()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.turns[id] = &liveTurn{msg: msg, running: true}
	g.ring = append(g.ring, id)
	for len(g.ring) > maxLiveTurns {
		delete(g.turns, g.ring[0])
		g.ring = g.ring[1:]
	}
	return id
}

// rememberCard keeps the last rendered payload so the debug endpoint can show
// exactly what was sent to Feishu.
func (g *Gateway) rememberCard(payload []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastCard = payload
}

// LastCard returns the most recently rendered card payload.
func (g *Gateway) LastCard() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastCard
}

// TurnInfo describes a turn a card button can still act on.
type TurnInfo struct {
	ID      string `json:"id"`
	CardID  string `json:"card_id"`
	Running bool   `json:"running"`
	Text    string `json:"text"`
}

// LiveTurns lists turns that are running or awaiting retry.
func (g *Gateway) LiveTurns() []TurnInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]TurnInfo, 0, len(g.ring))
	for _, id := range g.ring {
		entry := g.turns[id]
		if entry == nil {
			continue
		}
		out = append(out, TurnInfo{
			ID: id, CardID: entry.cardID, Running: entry.running, Text: entry.msg.Text,
		})
	}
	return out
}

func (g *Gateway) setTurnCard(id, cardID string) {
	if id == "" || cardID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry := g.turns[id]; entry != nil {
		entry.cardID = cardID
		log.Printf("gateway: turn %s card=%s", id, cardID)
	}
}

func (g *Gateway) finishTurn(id string, failed bool) {
	if id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.turns[id]
	if entry == nil {
		return
	}
	// Only a failed turn stays around, since retry is its only remaining action.
	if !failed {
		delete(g.turns, id)
		return
	}
	entry.running = false
}

func (g *Gateway) handleStopAction(action feishu.CardAction) feishu.CardToast {
	g.mu.Lock()
	entry := g.turns[action.RequestID]
	g.mu.Unlock()
	if entry == nil || !entry.running {
		return feishu.CardToast{Type: "info", Content: g.text.T(i18n.TurnActionExpired)}
	}
	if entry.msg.SenderOpenID != "" && action.OpenID != entry.msg.SenderOpenID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	cancel := entry.msg
	cancel.Text = string(protocol.CommandCancel)
	cancel.Images, cancel.ImageKeys, cancel.Quote = nil, nil, ""
	go g.process(cancel)
	return feishu.CardToast{Type: "success", Content: g.text.T(i18n.TurnStopRequested)}
}

// handleRecoverAction turns the tap into the words the user would have
// typed: a synthesized "/history 1" from this chat. The button carries the
// conversation id itself because a completed turn has already left the
// registry, and the shortcut must outlive it.
func (g *Gateway) handleRecoverAction(action feishu.CardAction) feishu.CardToast {
	if action.RequestID == "" {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalMalformed)}
	}
	msg := feishu.InboundMessage{
		ChatID:         action.ChatID,
		ConversationID: action.RequestID,
		MessageID:      action.MessageID,
		SenderOpenID:   action.OpenID,
		Text:           string(protocol.CommandHistory) + " 1",
	}
	go g.process(msg)
	return feishu.CardToast{Type: "success", Content: g.text.T(i18n.TurnStopRequested)}
}

func (g *Gateway) handleRetryAction(action feishu.CardAction) feishu.CardToast {
	g.mu.Lock()
	entry := g.turns[action.RequestID]
	if entry != nil && !entry.running {
		delete(g.turns, action.RequestID)
	}
	g.mu.Unlock()
	if entry == nil || entry.running {
		return feishu.CardToast{Type: "info", Content: g.text.T(i18n.TurnActionExpired)}
	}
	if entry.msg.SenderOpenID != "" && action.OpenID != entry.msg.SenderOpenID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	go func() {
		// Recall first so the failed card does not linger next to its replacement.
		g.recall(entry.cardID)
		g.process(entry.msg)
	}()
	return feishu.CardToast{Type: "success", Content: g.text.T(i18n.TurnRetryStarted)}
}

func (g *Gateway) recall(cardID string) {
	if cardID == "" {
		return
	}
	r, ok := g.ch.(recaller)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.DeleteMessage(ctx, cardID); err != nil {
		log.Printf("gateway: recall card failed: %v", err)
	}
}

func (g *Gateway) handleApprovalAction(action feishu.CardAction) feishu.CardToast {
	if action.Decision != "allow" && action.Decision != "deny" {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalMalformed)}
	}
	g.mu.Lock()
	pending := g.asks[action.RequestID]
	g.mu.Unlock()
	if pending == nil {
		return feishu.CardToast{Type: "info", Content: g.text.T(i18n.ApprovalExpired)}
	}
	if pending.openID != "" && action.OpenID != pending.openID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	if pending.cardID != "" && action.MessageID != "" && action.MessageID != pending.cardID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	select {
	case pending.done <- action.Decision:
	default:
	}
	if action.Decision == "allow" {
		return feishu.CardToast{Type: "success", Content: g.text.T(i18n.ApprovalAllowed)}
	}
	return feishu.CardToast{Type: "info", Content: g.text.T(i18n.ApprovalRejected)}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
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

// askQuestion parks the turn on a card the user taps to answer. It mirrors
// askPermission because it is the same interaction: the agent is blocked
// until a human decides, and a silent timeout has to resolve to something
// rather than hanging the prompt.
func (g *Gateway) askQuestion(ctx context.Context, ui *turnUI, q view.Question) (view.Answer, error) {
	ui.mu.Lock()
	cardID := ui.cardID
	closed := ui.closed || ui.listen || ui.fallback
	openID := ui.msg.SenderOpenID
	ui.mu.Unlock()
	if closed || cardID == "" {
		return view.Answer{}, nil
	}
	id := newRequestID()
	pending := &pendingAsk{openID: openID, cardID: cardID, done: make(chan string, 1)}
	g.mu.Lock()
	g.asks[id] = pending
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.asks, id)
		g.mu.Unlock()
		ui.setQuestion(nil)
	}()
	q.RequestID = id
	ui.setQuestion(&q)
	log.Printf("gateway: question %s pending: %d choices", id, len(q.Choices))
	timer := time.NewTimer(approvalTimeout)
	defer timer.Stop()
	select {
	case choice := <-pending.done:
		return view.Answer{Value: choice}, nil
	case <-timer.C:
		// Unanswered is a real answer here: the agent gets "cancelled" and
		// decides for itself, rather than the prompt stalling to its own
		// timeout with nothing on screen.
		log.Printf("gateway: question %s timed out", id)
		return view.Answer{}, nil
	case <-ctx.Done():
		return view.Answer{}, ctx.Err()
	}
}

func (g *Gateway) handleQuestionAction(action feishu.CardAction) feishu.CardToast {
	if action.Decision == "" {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalMalformed)}
	}
	g.mu.Lock()
	pending := g.asks[action.RequestID]
	g.mu.Unlock()
	if pending == nil {
		return feishu.CardToast{Type: "info", Content: g.text.T(i18n.ApprovalExpired)}
	}
	if pending.openID != "" && action.OpenID != pending.openID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	if pending.cardID != "" && action.MessageID != "" && action.MessageID != pending.cardID {
		return feishu.CardToast{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	select {
	case pending.done <- action.Decision:
	default:
	}
	return feishu.CardToast{Type: "success", Content: g.text.T(i18n.ApprovalAllowed)}
}
