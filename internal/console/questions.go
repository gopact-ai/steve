package console

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

func copyQuestion(q consoleapi.PendingQuestion) consoleapi.PendingQuestion {
	q.Options = append([]consoleapi.QuestionOption(nil), q.Options...)
	if q.Answer != nil {
		answer := *q.Answer
		q.Answer = &answer
	}
	return q
}

func (s *Service) Questions(conversation string) []consoleapi.PendingQuestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	questions := []consoleapi.PendingQuestion{}
	for _, q := range s.questions {
		if q.Principal == s.owner && (conversation == "" || q.Conversation == conversationID(conversation)) {
			questions = append(questions, copyQuestion(q))
		}
	}
	sort.Slice(questions, func(i, j int) bool { return questions[i].CreatedAt.Before(questions[j].CreatedAt) })
	return questions
}

// AnswerQuestion uses the authenticated console owner's identity, never a
// principal supplied in a request body. Decisions bypass the work queue.
func (s *Service) AnswerQuestion(ctx context.Context, id string, answer consoleapi.QuestionAnswer) (consoleapi.PendingQuestion, error) {
	return s.AnswerQuestionAs(ctx, s.owner, id, answer)
}

func (s *Service) AnswerQuestionAs(ctx context.Context, principal, id string, answer consoleapi.QuestionAnswer) (consoleapi.PendingQuestion, error) {
	if err := ctx.Err(); err != nil {
		return consoleapi.PendingQuestion{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.questions[id]
	if !ok {
		return q, consoleapi.ErrQuestionNotFound
	}
	if principal == "" || principal != s.owner || q.Principal != principal {
		return consoleapi.PendingQuestion{}, consoleapi.ErrQuestionForbidden
	}
	if answer.CommandID == "" || len(answer.CommandID) > 200 {
		return q, consoleapi.ErrInvalidAnswer
	}
	if q.Answer != nil && *q.Answer == answer {
		return copyQuestion(q), nil
	}
	if q.State != "pending" {
		return copyQuestion(q), consoleapi.ErrQuestionConflict
	}
	if !q.Deadline.After(time.Now()) {
		if err := s.resolveQuestionLocked(q, "expired", nil); err != nil {
			return q, err
		}
		return copyQuestion(s.questions[id]), consoleapi.ErrQuestionConflict
	}
	state := "answered"
	switch answer.Decision {
	case "accept":
		found := false
		for _, option := range q.Options {
			if option.ID == answer.Choice {
				found = true
				if strings.HasPrefix(option.Kind, "reject") {
					state = "declined"
				}
				break
			}
		}
		if !found {
			return q, consoleapi.ErrInvalidAnswer
		}
	case "decline", "cancel":
		if answer.Choice != "" {
			return q, consoleapi.ErrInvalidAnswer
		}
		state = map[string]string{"decline": "declined", "cancel": "cancelled"}[answer.Decision]
	default:
		return q, consoleapi.ErrInvalidAnswer
	}
	if err := s.resolveQuestionLocked(q, state, &answer); err != nil {
		return q, err
	}
	return copyQuestion(s.questions[id]), nil
}

func (s *Service) resolveQuestionLocked(q consoleapi.PendingQuestion, state string, answer *consoleapi.QuestionAnswer) error {
	previous := q
	q.State, q.Answer, q.UpdatedAt = state, answer, time.Now().UTC()
	s.questions[q.ID] = q
	if err := s.save(); err != nil {
		s.questions[q.ID] = previous
		return err
	}
	if waiter := s.questionWaiters[q.ID]; waiter != nil {
		close(waiter)
		delete(s.questionWaiters, q.ID)
	}
	s.publishQuestion(q)
	return nil
}

func (s *Service) publishQuestion(q consoleapi.PendingQuestion) {
	if s.model != nil {
		s.model.Publish(readmodel.Event{At: q.UpdatedAt, Kind: "console.question", Conversation: q.Conversation, ExchangeID: q.ExchangeID, Text: q.ID})
	}
}

func (s *Service) awaitQuestion(ctx context.Context, q consoleapi.PendingQuestion) (consoleapi.PendingQuestion, error) {
	if err := ctx.Err(); err != nil {
		return q, err
	}
	if len(q.Options) == 0 || len(q.Options) > 64 || len(q.Message) > 64<<10 || len(q.Title) > 4096 {
		return q, consoleapi.ErrInvalidAnswer
	}
	seen := map[string]bool{}
	for _, option := range q.Options {
		if option.ID == "" || len(option.ID) > 4096 || len(option.Label) > 4096 || len(option.Description) > 16<<10 || seen[option.ID] {
			return q, consoleapi.ErrInvalidAnswer
		}
		seen[option.ID] = true
	}
	if q.Project == "" {
		if current, err := s.Context(ctx, q.Conversation); err == nil && current.Project != nil {
			q.Project = current.Project.ID
		}
	}
	q.ID, q.State, q.Principal = "q"+strings.TrimPrefix(newReplyID(), "r"), "pending", s.owner
	q.CreatedAt = time.Now().UTC()
	q.UpdatedAt, q.Deadline = q.CreatedAt, q.CreatedAt.Add(s.questionTimeout)
	waiter := make(chan struct{})
	s.mu.Lock()
	s.questions[q.ID], s.questionWaiters[q.ID] = q, waiter
	if err := s.save(); err != nil {
		delete(s.questions, q.ID)
		delete(s.questionWaiters, q.ID)
		s.mu.Unlock()
		return q, err
	}
	s.publishQuestion(q)
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(q.Deadline))
	defer timer.Stop()
	state := ""
	select {
	case <-waiter:
	case <-ctx.Done():
		state = "cancelled"
	case <-timer.C:
		state = "expired"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q = s.questions[q.ID]
	if q.State == "pending" && state != "" {
		if err := s.resolveQuestionLocked(q, state, nil); err != nil {
			return q, err
		}
		q = s.questions[q.ID]
	}
	return copyQuestion(q), nil
}

func (s *Service) askPermission(ctx context.Context, base consoleapi.PendingQuestion, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	base.Kind, base.Title, base.Message, base.Required = "permission", ask.ToolName, ask.Reason, true
	base.SessionID, base.Generation, base.ToolCallID = ask.SessionID, ask.Generation, ask.ToolCallID
	for _, option := range ask.Options {
		base.Options = append(base.Options, consoleapi.QuestionOption{ID: string(option.OptionID), Label: option.Name, Kind: string(option.Kind)})
	}
	q, err := s.awaitQuestion(ctx, base)
	if err != nil {
		return permission.Choose(false, ask.Options), err
	}
	if q.Answer != nil && q.Answer.Decision == "accept" {
		return acp.SelectedRequestPermissionOutcome(acp.PermissionOptionID(q.Answer.Choice)), nil
	}
	return permission.Choose(false, ask.Options), nil
}

func (s *Service) askUser(ctx context.Context, base consoleapi.PendingQuestion, question view.Question) (view.Answer, error) {
	base.Kind, base.Title, base.Message, base.Required = "question", question.Title, question.Message, question.Required
	base.SessionID, base.Generation = question.SessionID, question.Generation
	if question.Kind != "" {
		base.Kind = question.Kind
	}
	for _, option := range question.Choices {
		base.Options = append(base.Options, consoleapi.QuestionOption{ID: option.Value, Label: option.Label, Description: option.Detail})
	}
	q, err := s.awaitQuestion(ctx, base)
	if err != nil {
		return view.Answer{}, err
	}
	if q.Answer != nil && q.Answer.Decision == "accept" {
		return view.Answer{Value: q.Answer.Choice}, nil
	}
	if q.Answer != nil && q.Answer.Decision == "decline" {
		return view.Answer{Decision: "decline"}, nil
	}
	return view.Answer{}, nil
}

func (s *Service) VerbsFor(ctx context.Context) []consoleapi.Verb {
	if aware, ok := s.handler.(interface {
		VerbsFor(context.Context) []turn.Verb
	}); ok {
		out := []consoleapi.Verb{}
		for _, v := range aware.VerbsFor(ctx) {
			out = append(out, consoleapi.Verb{Command: v.Command, Args: v.Args, Summary: v.Summary})
		}
		return out
	}
	return s.Verbs()
}
