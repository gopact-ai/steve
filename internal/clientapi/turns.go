package clientapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/conversationapi"
	"github.com/gopact-ai/steve/internal/material"
)

func (h *Handler) startTurn(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	var req StartTurn
	if err := decode(w, r, 1<<20, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.CommandID) == "" {
		writeError(w, badRequest("command_id is required"))
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Refs) == 0 {
		writeError(w, badRequest("input is required"))
		return
	}
	submission := conversationapi.Submission{
		Conversation: threadID(req.Thread), Input: req.Input, CommandID: req.CommandID,
		ExpectedProject: req.ExpectedProject, Locale: req.Locale, RewindTo: req.RewindTo,
	}
	if err := h.writable(r, submission.Conversation); err != nil {
		writeError(w, err)
		return
	}
	for _, quote := range req.Quotes {
		if err := h.writable(r, quote.Thread); err != nil {
			writeError(w, err)
			return
		}
		submission.Quotes = append(submission.Quotes, conversationapi.QuoteRef{Conversation: quote.Thread, ReplyID: quote.Item})
	}
	for _, ref := range req.Refs {
		submission.Refs = append(submission.Refs, toMaterialRef(ref))
	}
	exchange, err := c.Submit(r.Context(), submission)
	if err != nil {
		writeError(w, projectConflict(r.Context(), c, submission, err))
		return
	}
	writeJSON(w, toTurn(exchange))
}

// projectConflict names a refusal caused by the thread being bound to
// another project than the client expected. The console refuses such a
// submission without a sentinel error, so the binding is read again once
// the submission has failed; an accepted command replayed later is still
// answered from its receipt, as the console answers it.
func projectConflict(ctx context.Context, c consoleapi.Console, s conversationapi.Submission, err error) error {
	if s.ExpectedProject == "" || errors.Is(err, consoleapi.ErrCommandConflict) || errors.Is(err, consoleapi.ErrConsoleClosing) {
		return err
	}
	state, cerr := c.Context(ctx, s.Conversation)
	if cerr != nil {
		return err
	}
	bound := ""
	if state.Project != nil {
		bound = state.Project.ID
	}
	if bound != s.ExpectedProject {
		return refuse(http.StatusConflict, CodeProjectConflict, fmt.Errorf("expected project %s, but the thread is bound to %s", s.ExpectedProject, bound))
	}
	return err
}

func toMaterialRef(ref MaterialRef) material.Ref {
	out := material.Ref{ID: ref.ID}
	if s := ref.Selector; s != nil {
		out.Selector = &material.Selector{Kind: s.Kind, Start: s.Start, End: s.End, Quote: s.Quote}
		if s.Rect != nil {
			out.Selector.Rect = &material.Rect{X: s.Rect.X, Y: s.Rect.Y, Width: s.Rect.Width, Height: s.Rect.Height}
		}
	}
	return out
}

// ownedTurn finds the turn id names and checks its thread may be acted in.
func (h *Handler) ownedTurn(r *http.Request, c consoleapi.Console, id string) (conversationapi.Exchange, error) {
	conversation, found := c.ExchangeConversation(id)
	if !found {
		return conversationapi.Exchange{}, consoleapi.ErrExchangeNotFound
	}
	if err := h.writable(r, conversation); err != nil {
		return conversationapi.Exchange{}, err
	}
	for _, e := range c.Queue(conversation) {
		if e.ID == id {
			return e, nil
		}
	}
	return conversationapi.Exchange{}, consoleapi.ErrExchangeNotFound
}

// steerTurn starts a queued turn now, interrupting the one running.
func (h *Handler) steerTurn(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	if _, err := h.ownedTurn(r, c, id); err != nil {
		writeError(w, err)
		return
	}
	exchange, err := c.Steer(r.Context(), id)
	respond(w, toTurn(exchange), err)
}

// interruptTurn removes a queued turn, or stops a running one with the
// console's /cancel control. A turn that has ended is a conflict.
func (h *Handler) interruptTurn(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	var req Interrupt
	if err := decode(w, r, 1<<12, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.CommandID) == "" {
		writeError(w, badRequest("command_id is required"))
		return
	}
	exchange, err := h.ownedTurn(r, c, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	turn := toTurn(exchange)
	switch turn.State {
	case TurnQueued:
		if err := c.DeleteQueued(exchange.ID); err != nil {
			writeError(w, err)
			return
		}
		turn.State = TurnCancelled
		writeJSON(w, InterruptResult{Turn: turn})
	case TurnRunning:
		control, err := c.Submit(r.Context(), conversationapi.Submission{Conversation: exchange.Conversation, Input: "/cancel", CommandID: req.CommandID})
		if err != nil {
			writeError(w, err)
			return
		}
		stop := toTurn(control)
		writeJSON(w, InterruptResult{Turn: turn, Control: &stop})
	default:
		writeError(w, refuse(http.StatusConflict, CodeConflict, fmt.Errorf("turn %s has already ended: %s", exchange.ID, turn.State)))
	}
}

func (h *Handler) answerQuestion(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	var req QuestionAnswer
	if err := decode(w, r, 64<<10, &req); err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	for _, q := range c.Questions("") {
		if q.ID == id {
			if err := h.writable(r, q.Conversation); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	question, err := c.AnswerQuestion(r.Context(), id, conversationapi.QuestionAnswer{CommandID: req.CommandID, Decision: req.Decision, Choice: req.Choice, Text: req.Text})
	respond(w, AnswerResult{Question: toQuestion(question)}, err)
}
