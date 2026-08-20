package gateway

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/turn"
)

type cardPoster interface {
	ReplyCard(context.Context, string, []byte) (string, error)
	PatchCard(context.Context, string, []byte) error
}

var cardMinInterval = time.Second

type turnUI struct {
	g      *Gateway
	msg    feishu.InboundMessage
	listen bool
	copy   card.Copy

	mu        sync.Mutex
	state     card.Turn
	cardID    string
	fallback  bool
	closed    bool
	dirty     bool
	lastPatch time.Time
	timer     *time.Timer
	clock     *time.Timer
	wg        sync.WaitGroup
	reaction  string
	turnID    string
}

func (g *Gateway) newTurnUI(msg feishu.InboundMessage, listen bool) *turnUI {
	now := time.Now()
	ui := &turnUI{
		g:      g,
		msg:    msg,
		listen: listen,
		copy: card.Copy{
			Title:          g.text.T(i18n.CardTitle),
			Running:        g.text.T(i18n.CardRunning),
			Completed:      g.text.T(i18n.CardCompleted),
			Failed:         g.text.T(i18n.CardFailed),
			Cancelled:      g.text.T(i18n.CardCancelled),
			EarlierTools:   g.text.T(i18n.CardEarlierTools),
			Execution:      g.text.T(i18n.CardExecution),
			Input:          g.text.T(i18n.CardInput),
			Output:         g.text.T(i18n.CardOutput),
			Context:        g.text.T(i18n.CardContext),
			In:             g.text.T(i18n.CardIn),
			Out:            g.text.T(i18n.CardOut),
			Hit:            g.text.T(i18n.CardHit),
			Write:          g.text.T(i18n.CardWrite),
			Awaiting:       g.text.T(i18n.CardAwaiting),
			Waking:         g.text.T(i18n.CardWaking),
			Stop:           g.text.T(i18n.CardStop),
			Retry:          g.text.T(i18n.CardRetry),
			ApprovalTitle:  g.text.T(i18n.CardApprovalTitle),
			ApprovalTool:   g.text.T(i18n.CardApprovalTool),
			ApprovalReason: g.text.T(i18n.CardApprovalReason),
			ApprovalRule:   g.text.T(i18n.CardApprovalRule),
			AllowOnce:      g.text.T(i18n.CardAllowOnce),
			Deny:           g.text.T(i18n.CardDeny),
		},
		state: card.Turn{Status: card.StatusRunning, Phase: card.PhaseWaking, StartedAt: now, UpdatedAt: now},
	}
	if listen {
		return ui
	}
	ui.turnID = g.registerTurn(msg)
	ui.state.TurnID = ui.turnID
	ui.reaction = g.ack(msg.MessageID)
	if poster, ok := g.ch.(cardPoster); ok && msg.MessageID != "" {
		payload := card.Render(ui.state, ui.copy)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		id, err := poster.ReplyCard(ctx, msg.MessageID, payload)
		cancel()
		if err == nil && id != "" {
			ui.cardID = id
			g.setTurnCard(ui.turnID, id)
			ui.lastPatch = time.Now()
			ui.clock = time.AfterFunc(cardMinInterval, ui.tick)
			return ui
		}
		if err != nil {
			log.Printf("gateway: card start failed: %v", err)
		}
		ui.fallback = true
	}
	return ui
}

func (u *turnUI) setPhase(p card.Phase) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.listen || u.fallback || u.cardID == "" || u.state.Phase == p {
		return
	}
	u.state.Phase = p
	u.state.UpdatedAt = time.Now()
	u.dirty = true
	u.scheduleLocked()
}

func (u *turnUI) setApproval(a *card.Approval) {
	u.mu.Lock()
	if u.closed || u.listen || u.fallback || u.cardID == "" {
		u.mu.Unlock()
		return
	}
	u.state.Approval = a
	u.state.UpdatedAt = time.Now()
	u.dirty = true
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	u.mu.Unlock()
	u.flush()
}

func (u *turnUI) progress(p card.Progress) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.listen || u.fallback || u.cardID == "" {
		return
	}
	u.state.Answer = p.Answer
	u.state.Reasoning = p.Reasoning
	u.state.Tools = append([]card.Tool(nil), p.Tools...)
	u.state.Usage = p.Usage
	u.state.UpdatedAt = time.Now()
	u.dirty = true
	u.scheduleLocked()
}

func (u *turnUI) tick() {
	u.mu.Lock()
	u.clock = nil
	if u.closed || u.fallback || u.cardID == "" {
		u.mu.Unlock()
		return
	}
	u.state.UpdatedAt = time.Now()
	u.dirty = true
	u.scheduleLocked()
	u.clock = time.AfterFunc(cardMinInterval, u.tick)
	u.mu.Unlock()
}

func (u *turnUI) scheduleLocked() {
	if u.timer != nil {
		return
	}
	wait := cardMinInterval - time.Since(u.lastPatch)
	if wait < 0 {
		wait = 0
	}
	u.timer = time.AfterFunc(wait, u.flush)
}

func (u *turnUI) flush() {
	u.mu.Lock()
	u.timer = nil
	if u.closed || u.fallback || u.cardID == "" || !u.dirty {
		u.mu.Unlock()
		return
	}
	payload := card.Render(u.state, u.copy)
	u.g.rememberCard(payload)
	id := u.cardID
	u.dirty = false
	u.lastPatch = time.Now()
	u.wg.Add(1)
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := u.g.ch.(cardPoster).PatchCard(ctx, id, payload)
	cancel()
	u.wg.Done()

	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		log.Printf("gateway: card patch failed: %v", err)
		u.fallback = true
		return
	}
	if u.dirty && !u.closed {
		u.scheduleLocked()
	}
}

func (u *turnUI) finish(result turn.Result, err error) {
	if u.listen && (err != nil || result.Text == "") {
		u.mu.Lock()
		u.closed = true
		if u.timer != nil {
			u.timer.Stop()
			u.timer = nil
		}
		if u.clock != nil {
			u.clock.Stop()
			u.clock = nil
		}
		u.mu.Unlock()
		return
	}
	text, status := u.finalText(result, err)
	u.mu.Lock()
	u.closed = true
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	if u.clock != nil {
		u.clock.Stop()
		u.clock = nil
	}
	u.state.Approval = nil
	u.state.Status = status
	u.state.Title = result.Title
	u.state.Fields = append([]card.Field(nil), result.Fields...)
	u.state.Answer = text
	u.state.UpdatedAt = time.Now()
	if status == card.StatusFailed {
		u.state.Error = text
		u.state.Answer = ""
		u.state.Fields = nil
	} else if len(u.state.Fields) > 0 {
		u.state.Answer = ""
	}
	cardID := u.cardID
	fallback := u.fallback || cardID == ""
	u.mu.Unlock()
	u.wg.Wait()
	// Retry needs the card to keep its button, so a failed turn stays live.
	retryable := status == card.StatusFailed || status == card.StatusCancelled
	if !retryable {
		u.mu.Lock()
		u.state.TurnID = ""
		u.mu.Unlock()
	}
	u.g.finishTurn(u.turnID, retryable)

	if !fallback && u.patchFinal() {
		u.g.unack(u.msg.MessageID, u.reaction)
		return
	}
	if text != "" {
		u.g.reply(u.msg.MessageID, text)
	}
	u.g.unack(u.msg.MessageID, u.reaction)
}

func (u *turnUI) patchFinal() bool {
	poster, ok := u.g.ch.(cardPoster)
	if !ok || u.cardID == "" {
		return false
	}
	u.mu.Lock()
	payload := card.Render(u.state, u.copy)
	id := u.cardID
	u.mu.Unlock()
	u.g.rememberCard(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := poster.PatchCard(ctx, id, payload); err != nil {
		log.Printf("gateway: card finish failed: %v", err)
		return false
	}
	return true
}

func (u *turnUI) finalText(result turn.Result, err error) (string, card.Status) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return u.g.text.T(i18n.TurnCanceled), card.StatusCancelled
		}
		var userErr turn.UserError
		if errors.As(err, &userErr) {
			return userErr.Text, card.StatusFailed
		}
		return u.g.text.T(i18n.AgentFailed), card.StatusFailed
	}
	out := result.Text
	if out == "" {
		out = u.g.text.T(i18n.EmptyReply)
	}
	if u.fallback && len(result.Activity) > 0 {
		out += "\n\n---\n" + strings.Join(result.Activity, "\n")
	}
	return u.g.truncateRunes(out, maxReplyRunes), card.StatusCompleted
}
