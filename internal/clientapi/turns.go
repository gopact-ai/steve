package clientapi

import (
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
		if err := quotable(c, quote); err != nil {
			writeError(w, err)
			return
		}
		submission.Quotes = append(submission.Quotes, conversationapi.QuoteRef{Conversation: quote.Thread, ReplyID: quote.Item})
	}
	for _, ref := range req.Refs {
		submission.Refs = append(submission.Refs, toMaterialRef(ref))
	}
	// The console names a project mismatch with its own error, so any
	// other refusal (a bad quote, a material, a conflict) keeps its code.
	exchange, err := c.Submit(r.Context(), submission)
	respond(w, toTurn(exchange), err)
}

// quotable refuses a quote of an item the console cannot carry: only an
// assistant reply's message, a milestone or a notice is quoted, by the
// item ID the thread lists. Items derived from a reply (its reasoning,
// tools, steps, changes), questions and the user's own messages are not.
// An ID the thread does not hold is left to the console, which refuses
// it as no longer in the transcript.
func quotable(c consoleapi.Console, quote Quote) error {
	if strings.Contains(quote.Item, "#") || strings.HasPrefix(quote.Item, "question:") {
		return badRequest(fmt.Sprintf("item %s cannot be quoted: quote the reply's message item instead", quote.Item))
	}
	for _, line := range c.Replies(threadID(quote.Thread)) {
		if line.ID != quote.Item {
			continue
		}
		if line.Kind == "sent" {
			return badRequest(fmt.Sprintf("item %s is a user message and cannot be quoted", quote.Item))
		}
		if line.Silent {
			return badRequest(fmt.Sprintf("item %s is not shown in the thread and cannot be quoted", quote.Item))
		}
		return nil
	}
	return nil
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

// interruptTurn removes a queued turn, or stops a running one with a
// /cancel control bound to that turn: the stop reaches no other turn, and
// does nothing when the turn ends before the stop runs. Retrying the same
// command_id returns the same control, even after the turn has ended. A
// new interrupt for a turn that has ended is a conflict.
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
	if exchange.State == conversationapi.ExchangeQueued {
		err := c.DeleteQueued(exchange.ID)
		if err == nil {
			turn := toTurn(exchange)
			turn.State = TurnCancelled
			writeJSON(w, InterruptResult{Turn: turn})
			return
		}
		if !errors.Is(err, consoleapi.ErrExchangeNotQueued) {
			writeError(w, err)
			return
		}
		// It started meanwhile: stop it as a running turn.
	}
	// The console admits the control only while the target has not ended,
	// and answers a retry of an admitted control from its receipt.
	control, err := c.Submit(r.Context(), conversationapi.Submission{Conversation: exchange.Conversation, Input: "/cancel", CommandID: req.CommandID, StopTarget: exchange.ID})
	if err != nil {
		writeError(w, err)
		return
	}
	turn := toTurn(exchange)
	if current, err := h.ownedTurn(r, c, exchange.ID); err == nil {
		turn = toTurn(current)
	}
	stop := toTurn(control)
	writeJSON(w, InterruptResult{Turn: turn, Control: &stop})
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
