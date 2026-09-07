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
	return s.askUser(ctx, binding, question)
}

// RequestNativePermission requires the original node's explicit permission
// options. The service derives the principal; a caller cannot supply an owner.
func (s *Service) RequestNativePermission(ctx context.Context, binding consoleapi.PendingQuestion, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	if err := nativeQuestionMatches(ctx, binding, ask.SessionID, ask.RequestID); err != nil {
		return permission.Choose(false, ask.Options), err
	}
	return s.askPermission(ctx, binding, ask)
}
