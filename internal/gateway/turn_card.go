package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/turn"
)

// cardMinInterval coalesces changed progress snapshots into at most one
// ordinary patch per interval. No timer runs when there is nothing new.
var cardMinInterval = 3 * time.Second

type turnUI struct {
	g *Gateway
	// ch is the gateway's channel when the turn began; the turn uses it
	// throughout, even if a channel is bound while it runs.
	ch         Channel
	msg        feishu.InboundMessage
	listen     bool
	resultOnly bool
	copy       card.Copy

	// sendMu serializes card delivery. Acquire it before mu when both are
	// needed, and render only after admission so queued sends use fresh state.
	sendMu    sync.Mutex
	mu        sync.Mutex
	state     card.Turn
	cardID    string
	fallback  bool
	closed    bool
	dirty     bool
	lastPatch time.Time
	timer     *time.Timer
	reaction  string
	turnID    string
	style     string
}

func (g *Gateway) newTurnUI(ch Channel, msg feishu.InboundMessage, listen bool) *turnUI {
	now := time.Now()
	ui := &turnUI{
		g:      g,
		ch:     ch,
		msg:    msg,
		listen: listen,
		copy: card.Copy{
			Title:          g.text.T(i18n.CardTitle),
			Running:        g.text.T(i18n.CardRunning),
			Completed:      g.text.T(i18n.CardCompleted),
			Failed:         g.text.T(i18n.CardFailed),
			Cancelled:      g.text.T(i18n.CardCancelled),
			EarlierTools:   g.text.T(i18n.CardEarlierTools),
			Partial:        g.text.T(i18n.CardPartial),
			Execution:      g.text.T(i18n.CardExecution),
			Plan:           g.text.T(i18n.CardPlan),
			EarlierSteps:   g.text.T(i18n.CardEarlierSteps),
			Input:          g.text.T(i18n.CardInput),
			Output:         g.text.T(i18n.CardOutput),
			Context:        g.text.T(i18n.CardContext),
			In:             g.text.T(i18n.CardIn),
			Out:            g.text.T(i18n.CardOut),
			Hit:            g.text.T(i18n.CardHit),
			Write:          g.text.T(i18n.CardWrite),
			Awaiting:       g.text.T(i18n.CardAwaiting),
			Waking:         g.text.T(i18n.CardWaking),
			Finishing:      g.text.T(i18n.CardFinishing),
			Saving:         g.text.T(i18n.CardSaving),
			Stop:           g.text.T(i18n.CardStop),
			Retry:          g.text.T(i18n.CardRetry),
			ApprovalTitle:  g.text.T(i18n.CardApprovalTitle),
			ApprovalTool:   g.text.T(i18n.CardApprovalTool),
			ApprovalReason: g.text.T(i18n.CardApprovalReason),
			ApprovalRule:   g.text.T(i18n.CardApprovalRule),
			QuestionTitle:  g.text.T(i18n.CardQuestionTitle),
			QuestionHint:   g.text.T(i18n.CardQuestionHint),
			Recover:        g.text.T(i18n.CardRecover),
			SentTo:         g.text.T(i18n.CardSentTo),
			AllowOnce:      g.text.T(i18n.CardAllowOnce),
			Deny:           g.text.T(i18n.CardDeny),
		},
		state: card.Turn{
			Status: card.StatusRunning, Phase: card.PhaseWaking,
			Recipient: msg.SenderOpenID, StartedAt: now, UpdatedAt: now,
		},
	}
	if listen {
		return ui
	}
	ui.turnID = g.registerTurn(msg)
	ui.state.TurnID = ui.turnID
	ui.reaction = ack(ch, msg.MessageID)
	if ch != nil && msg.MessageID != "" {
		payload := card.Render(ui.state, ui.copy)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		id, err := ch.ReplyCard(ctx, msg.MessageID, payload)
		cancel()
		if err == nil && id != "" {
			ui.cardID = id
			g.setTurnCard(ui.turnID, id)
			ui.lastPatch = time.Now()
			return ui
		}
		if err != nil {
			slog.Error(fmt.Sprintf("gateway: card start failed: %v", err), "conversation", conversationID(ui.msg), "message", ui.msg.MessageID)
		}
		ui.fallback = true
	}
	return ui
}

// A retained result has no new live turn. Render only its final delivery,
// without an opener, reaction or native-progress worker.
func (g *Gateway) newResultUI(ch Channel, msg feishu.InboundMessage) *turnUI {
	ui := g.newTurnUI(ch, msg, true)
	ui.listen, ui.resultOnly = silentListen(msg), true
	return ui
}

// closeProgress stops admitting progress patches and returns only once an
// in-flight patch has been delivered or has failed.
func (u *turnUI) closeProgress() {
	u.stopProgress()
	// Admission is closed first: a queued flush checks closed after
	// acquiring sendMu, so it cannot send another patch even if its timer
	// already fired. Acquiring sendMu then waits for the one in flight.
	u.sendMu.Lock()
	defer u.sendMu.Unlock()
}

func (u *turnUI) stopProgress() {
	u.mu.Lock()
	u.closed = true
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	u.mu.Unlock()
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
	u.flushProgress(true)
}

func (u *turnUI) setQuestion(q *card.Question) {
	u.mu.Lock()
	if u.closed || u.listen || u.fallback || u.cardID == "" {
		u.mu.Unlock()
		return
	}
	u.state.Question = q
	u.state.UpdatedAt = time.Now()
	u.dirty = true
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	u.mu.Unlock()
	u.flushProgress(true)
}

// progress records every snapshot and schedules changed card content.
// Streaming text uses the same coalescing timer as tools and plans, so it
// reaches the card before finish without requiring a patch for every token.
func (u *turnUI) progress(p card.Progress) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.listen || u.fallback || u.cardID == "" {
		return
	}
	news := isMilestone(u.state, p)
	// Some execution paths report content without a phase callback. Content
	// proves execution has started; configuration and usage alone do not.
	// Never demote an explicit later phase such as finishing or saving.
	if u.state.Phase == card.PhaseWaking &&
		(p.Answer != "" || p.Reasoning != "" || len(p.Tools) > 0 || len(p.Plan) > 0) {
		u.state.Phase = card.PhaseRunning
		news = true
	}
	u.state.Answer = p.Answer
	u.state.Reasoning = p.Reasoning
	u.state.Tools = append([]card.Tool(nil), p.Tools...)
	u.state.Usage = p.Usage
	u.state.Plan = append([]card.Step(nil), p.Plan...)
	// Settings only ever become more complete during a turn, so a snapshot
	// taken before the agent reported its model must not blank them out.
	if !p.Settings.Empty() {
		u.state.Settings = p.Settings
		// Interim cards should wear the same tail as this card will; push
		// the identity line as soon as (and whenever) it becomes known.
		if u.g.gate != nil {
			if line := card.SettingsLine(p.Settings); line != "" && line != u.style {
				u.style = line
				u.g.gate.SetStyle(conversationID(u.msg), line)
			}
		}
	}
	u.state.UpdatedAt = time.Now()
	if !news {
		return
	}
	u.dirty = true
	u.scheduleLocked()
}

// isMilestone detects changed card content, not just tool/plan transitions.
// Compare settings as rendered: selector metadata is not visible, and an
// empty settings snapshot retains the identity already stored by progress.
func isMilestone(state card.Turn, next card.Progress) bool {
	if state.Answer != next.Answer || state.Reasoning != next.Reasoning ||
		!slices.Equal(state.Plan, next.Plan) ||
		!slices.EqualFunc(state.Tools, next.Tools, sameCardTool) ||
		!reflect.DeepEqual(state.Usage, next.Usage) {
		return true
	}
	return !next.Settings.Empty() && card.SettingsLine(state.Settings) != card.SettingsLine(next.Settings)
}

func sameCardTool(a, b card.Tool) bool {
	return a.ID == b.ID && a.Status == b.Status &&
		a.Kind == b.Kind && a.Name == b.Name && a.Detail == b.Detail &&
		a.Input == b.Input && a.Output == b.Output &&
		a.StartedAt.Equal(b.StartedAt) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		slices.EqualFunc(a.Children, b.Children, sameCardTool)
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
	u.flushProgress(false)
}

func (u *turnUI) flushProgress(immediate bool) {
	u.sendMu.Lock()
	defer u.sendMu.Unlock()

	u.mu.Lock()
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
	if u.closed || u.fallback || u.cardID == "" || !u.dirty {
		u.mu.Unlock()
		return
	}
	// A timer may have fired before a newer control patch acquired sendMu.
	// Recheck the floor at admission instead of sending that stale timer now.
	if !immediate && time.Since(u.lastPatch) < cardMinInterval {
		u.scheduleLocked()
		u.mu.Unlock()
		return
	}
	payload := card.Render(u.state, u.copy)
	u.g.rememberCard(payload)
	id := u.cardID
	u.dirty = false
	u.lastPatch = time.Now()
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := u.ch.PatchCard(ctx, id, payload)
	cancel()

	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		slog.Error(fmt.Sprintf("gateway: card patch failed: %v", err), "conversation", conversationID(u.msg), "card", u.cardID)
		u.fallback = true
		return
	}
	if u.dirty && !u.closed {
		u.scheduleLocked()
	}
}

func (u *turnUI) finish(result turn.Result, err error) (string, error) {
	u.stopProgress()
	u.sendMu.Lock()
	defer u.sendMu.Unlock()
	// Closing admission before waiting prevents late timers/control flushes
	// from sending after final. In-flight failures also settle fallback before
	// finalText and the delivery choice read it.
	if u.listen && (err != nil || result.Text == "") {
		return "silent-listen", nil
	}
	text, status := u.finalText(result, err)
	u.mu.Lock()
	u.state.Approval = nil
	u.state.Question = nil
	u.state.Status = status
	u.state.Title = result.Title
	u.state.Fields = append([]card.Field(nil), result.Fields...)
	u.state.Answer = text
	u.state.UpdatedAt = time.Now()
	if result.Recover && status == card.StatusCompleted {
		u.state.RecoverID = conversationID(u.msg)
	}
	// The answer renders inside the card as rich text. It went out as a
	// plain message for one iteration; that made markdown display as bare
	// symbols and split every turn into two mismatched artifacts. One card,
	// rich body, is the uniform shape.
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
	defer unack(u.ch, u.msg.MessageID, u.reaction)
	// Retry needs the card to keep its button, so a failed turn stays live.
	retryable := status == card.StatusFailed || status == card.StatusCancelled
	if !retryable {
		u.mu.Lock()
		u.state.TurnID = ""
		u.mu.Unlock()
	}
	// Interim cards sit below the opening card, so patching the answer into
	// that card would put the conclusion above the milestones that led to
	// it. Post the final card at the bottom instead and drop the opener:
	// the chat then reads in the order things actually happened.
	if !fallback && u.g.gate != nil && u.g.gate.Interim(conversationID(u.msg)) {
		if newID, repostErr := u.repostFinal(); repostErr == nil && newID != "" {
			u.g.setTurnCard(u.turnID, newID)
			u.g.finishTurn(u.turnID, retryable)
			recall(u.ch, cardID)
			return newID, nil
		} else if errors.Is(repostErr, channel.ErrOutcomeUnknown) {
			return "", repostErr
		}
	}
	u.g.finishTurn(u.turnID, retryable)
	if u.resultOnly && !u.listen && u.ch != nil {
		if id, err := u.repostFinal(); err == nil {
			return id, nil
		} else if errors.Is(err, channel.ErrOutcomeUnknown) {
			return "", err
		}
	}

	if !fallback {
		if patchErr := u.patchFinal(); patchErr == nil {
			return cardID, nil
		} else if errors.Is(patchErr, channel.ErrOutcomeUnknown) {
			return "", patchErr
		}
	}
	if text != "" {
		if u.ch == nil {
			return "", errors.New("gateway reply channel is not available")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if u.g.recoveryLedger != nil || u.resultOnly {
			id, err := u.ch.ReplyText(ctx, u.msg.MessageID, text)
			if err != nil {
				return "", noticeError(err)
			}
			if id != "" {
				return id, nil
			}
			return "", channel.ErrOutcomeUnknown
		}
		if err := u.ch.Reply(ctx, u.msg.MessageID, text); err != nil {
			return "", noticeError(err)
		}
		return "reply:" + u.msg.MessageID, nil
	}
	return "", errors.New("gateway final reply has no content")
}

// repostFinal sends the finished card as a new message and returns its id,
// or "" when the channel refused it — the caller then patches in place,
// which is worse-ordered but never loses the answer. Callers have a channel.
func (u *turnUI) repostFinal() (string, error) {
	if u.msg.MessageID == "" {
		return "", errors.New("gateway card repost is unavailable")
	}
	u.mu.Lock()
	payload := card.Render(u.state, u.copy)
	u.mu.Unlock()
	u.g.rememberCard(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := u.ch.ReplyCard(ctx, u.msg.MessageID, payload)
	if err != nil || id == "" {
		slog.Error(fmt.Sprintf("gateway: final card repost failed: %v", err), "conversation", conversationID(u.msg), "message", u.msg.MessageID)
		if err == nil {
			err = channel.ErrOutcomeUnknown
		}
		return "", noticeError(err)
	}
	u.mu.Lock()
	u.cardID = id
	u.mu.Unlock()
	return id, nil
}

// patchFinal needs a posted card, which implies a channel.
func (u *turnUI) patchFinal() error {
	if u.cardID == "" {
		return errors.New("gateway card patch is unavailable")
	}
	u.mu.Lock()
	payload := card.Render(u.state, u.copy)
	id := u.cardID
	u.mu.Unlock()
	u.g.rememberCard(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := u.ch.PatchCard(ctx, id, payload); err != nil {
		slog.Error(fmt.Sprintf("gateway: card finish failed: %v", err), "conversation", conversationID(u.msg), "card", id)
		return noticeError(err)
	}
	return nil
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
