package node

import (
	"context"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func (one *ownedSession) askUser(ctx context.Context, commandID string, q view.Question) (view.Answer, error) {
	result, err := one.waitQuestion(ctx, nodewire.SessionQuestion{CommandID: commandID, Question: q})
	if err != nil {
		return view.Answer{}, err
	}
	if result.Answer == nil {
		return view.Answer{}, nil
	}
	switch result.Answer.Decision {
	case "accept":
		return view.Answer{Value: result.Answer.Choice, Text: result.Answer.Text}, nil
	case "decline":
		return view.Answer{Decision: "decline"}, nil
	default:
		return view.Answer{}, nil
	}
}

func (one *ownedSession) askPermission(ctx context.Context, commandID string, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	q := view.Question{Kind: "permission", Title: ask.ToolName, Message: ask.Reason, Required: true, SessionID: ask.SessionID, Generation: ask.Generation}
	for _, option := range ask.Options {
		q.Choices = append(q.Choices, view.Choice{Value: string(option.OptionID), Label: option.Name})
	}
	result, err := one.waitQuestion(ctx, nodewire.SessionQuestion{CommandID: commandID, Question: q, Permission: &ask})
	if err != nil {
		return permission.Choose(false, ask.Options), err
	}
	if result.Answer != nil && result.Answer.Decision == "accept" {
		return acp.SelectedRequestPermissionOutcome(acp.PermissionOptionID(result.Answer.Choice)), nil
	}
	return permission.Choose(false, ask.Options), nil
}

func (one *ownedSession) waitQuestion(ctx context.Context, question nodewire.SessionQuestion) (nodewire.SessionQuestion, error) {
	if err := ctx.Err(); err != nil {
		return question, err
	}
	one.mu.Lock()
	if len(one.record.State.Questions) >= 256 {
		one.mu.Unlock()
		return question, sessionError("unavailable", "node session question retention limit reached")
	}
	question.ID = "nq_" + sessionHash([]any{one.record.State.ID, question.CommandID, one.record.State.Sequence + 1})
	question.State = "pending"
	question.CreatedAt = time.Now().UTC()
	question.Question.RequestID = question.ID
	if question.Question.Kind == "permission" {
		question.Question.AllowFreeText = false
	}
	next := one.copyLocked()
	next.State.Questions = append(next.State.Questions, question)
	waiter := make(chan struct{})
	if err := one.commitLocked(next); err != nil {
		one.mu.Unlock()
		return question, err
	}
	one.waiters[question.ID] = waiter
	one.mu.Unlock()
	select {
	case <-waiter:
	case <-ctx.Done():
	}
	one.mu.Lock()
	defer one.mu.Unlock()
	delete(one.waiters, question.ID)
	for i, q := range one.record.State.Questions {
		if q.ID != question.ID {
			continue
		}
		if q.State == "pending" {
			next := one.copyLocked()
			next.State.Questions[i].State = "interrupted"
			if err := one.commitLocked(next); err != nil {
				return question, err
			}
			return next.State.Questions[i], ctx.Err()
		}
		return q, nil
	}
	return question, sessionError("unavailable", "node question disappeared")
}

func (one *ownedSession) answer(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := one.admitLocked(req); err != nil {
		return nodewire.SessionState{}, err
	}
	if req.Answer == nil || !sessionNameValid(req.Answer.CommandID) {
		return nodewire.SessionState{}, sessionError("invalid", "answer requires an idempotent command")
	}
	for i, q := range one.record.State.Questions {
		if q.ID != req.QuestionID {
			continue
		}
		answer := *req.Answer
		if q.Answer != nil && *q.Answer == answer {
			return one.stateLocked(req.CommandID), nil
		}
		if q.State != "pending" || one.waiters[q.ID] == nil || q.CommandID != one.record.CurrentCommand {
			return nodewire.SessionState{}, sessionError("conflict", "question has no live callback or was already resolved")
		}
		switch answer.Decision {
		case "accept":
			if answer.Text != "" {
				if !q.Question.AllowFreeText || q.Question.Kind == "permission" || answer.Choice != "" || strings.TrimSpace(answer.Text) == "" || len(answer.Text) > 64<<10 {
					return nodewire.SessionState{}, sessionError("invalid", "question does not accept this text answer")
				}
			} else {
				found := false
				for _, choice := range q.Question.Choices {
					if answer.Choice == choice.Value {
						found = true
						break
					}
				}
				if !found {
					return nodewire.SessionState{}, sessionError("invalid", "answer is not an offered choice")
				}
			}
		case "decline", "cancel":
			if answer.Choice != "" || answer.Text != "" {
				return nodewire.SessionState{}, sessionError("invalid", "decline/cancel cannot supply an answer")
			}
		default:
			return nodewire.SessionState{}, sessionError("invalid", "unknown answer decision")
		}
		next := one.copyLocked()
		next.State.Questions[i].Answer = &answer
		next.State.Questions[i].State = "answered"
		if err := one.commitLocked(next); err != nil {
			return nodewire.SessionState{}, err
		}
		close(one.waiters[q.ID])
		delete(one.waiters, q.ID)
		return one.stateLocked(req.CommandID), nil
	}
	return nodewire.SessionState{}, sessionError("unavailable", "unknown node question")
}
