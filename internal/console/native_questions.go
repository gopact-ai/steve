package console

import (
	"context"
	"strings"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func nativeQuestionMatches(ctx context.Context, binding consoleapi.PendingQuestion, sessionID, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, sourceSession, sourceRequest, ok := harness.NativeQuestionSource(ctx)
	if !ok || binding.Conversation == "" || binding.Project == "" || binding.TaskID == "" || binding.AttemptID == "" ||
		!strings.HasPrefix(sessionID, "ns_") || !strings.HasPrefix(requestID, "nq_") ||
		binding.SessionID != sessionID || binding.RequestID != requestID || sourceSession != sessionID || sourceRequest != requestID ||
		binding.Project != source.ProjectID || binding.TaskID != source.TaskID || binding.AttemptID != source.AttemptID {
		return consoleapi.ErrInvalidAnswer
	}
	return nil
}

// RequestNativeQuestion exposes an authenticated node callback in its parent
// conversation. It creates no task, prompt or execution authorization.
func (s *Service) RequestNativeQuestion(ctx context.Context, binding consoleapi.PendingQuestion, question view.Question) (view.Answer, error) {
	if err := nativeQuestionMatches(ctx, binding, question.SessionID, question.RequestID); err != nil {
		return view.Answer{}, err
	}
	if question.Kind != "" && question.Kind != "question" && question.Kind != "permission" {
		return view.Answer{}, consoleapi.ErrInvalidAnswer
	}
	return s.askUser(ctx, binding, question, true)
}

// RequestNativePermission requires the original node's explicit permission
// options. The service derives the principal; a caller cannot supply an owner.
func (s *Service) RequestNativePermission(ctx context.Context, binding consoleapi.PendingQuestion, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	if err := nativeQuestionMatches(ctx, binding, ask.SessionID, ask.RequestID); err != nil {
		return permission.Choose(false, ask.Options), err
	}
	return s.askPermission(ctx, binding, ask, true)
}

// localQuestionMatches admits a question from an execution this hub runs
// itself. Its authority is the hub's own attempt record, which the caller
// passes as binding; a node-owned session and a node callback have their
// own entry and are refused here. The question is bound to the execution,
// not to an exchange, because the asker can outlive the turn that started it.
func localQuestionMatches(ctx context.Context, binding consoleapi.PendingQuestion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, _, native := harness.NativeQuestionSource(ctx); native || binding.Conversation == "" || binding.ExchangeID != "" ||
		binding.TaskID == "" || binding.AttemptID == "" || binding.SessionID == "" || strings.HasPrefix(binding.SessionID, "ns_") {
		return consoleapi.ErrInvalidAnswer
	}
	return nil
}

// RequestLocalQuestion puts a question from a hub-local execution — a
// delegated child or a plan step — before the owner in the conversation
// binding names, and waits until it is answered or the execution stops.
func (s *Service) RequestLocalQuestion(ctx context.Context, binding consoleapi.PendingQuestion, question view.Question) (view.Answer, error) {
	if err := localQuestionMatches(ctx, binding); err != nil {
		return view.Answer{}, err
	}
	if question.Kind != "" && question.Kind != "question" && question.Kind != "permission" {
		return view.Answer{}, consoleapi.ErrInvalidAnswer
	}
	return s.askUser(ctx, binding, localQuestion(question, binding.SessionID), true)
}

// RequestLocalPermission is RequestLocalQuestion for a tool permission.
func (s *Service) RequestLocalPermission(ctx context.Context, binding consoleapi.PendingQuestion, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	if err := localQuestionMatches(ctx, binding); err != nil {
		return permission.Choose(false, ask.Options), err
	}
	ask.SessionID, ask.RequestID = binding.SessionID, ""
	return s.askPermission(ctx, binding, ask, true)
}

// localQuestion records the execution's session, not the agent process's
// own session id, which a plugin profile may rename.
func localQuestion(q view.Question, session string) view.Question {
	q.SessionID, q.RequestID = session, ""
	return q
}
