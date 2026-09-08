package app

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func planQuestionBinding(ctx context.Context, cons *console.Service, tasks *task.Store, attempts *attempt.Service, sessionID, requestID string) (consoleapi.PendingQuestion, error) {
	source, nativeSession, nativeRequest, ok := harness.NativeQuestionSource(ctx)
	if !ok || sessionID != nativeSession || requestID != nativeRequest {
		return consoleapi.PendingQuestion{}, consoleapi.ErrInvalidAnswer
	}
	record, err := attempts.Get(ctx, source.AttemptID)
	if err != nil {
		return consoleapi.PendingQuestion{}, err
	}
	tracked, ok := tasks.Get(source.TaskID)
	if !ok || tracked.ProjectID != source.ProjectID || record.TaskID != source.TaskID || record.Project != source.ProjectID || record.Session != nativeSession || record.Node != source.NodeID || attempt.SessionExecutionEpoch(record) != source.ExecutionEpoch {
		return consoleapi.PendingQuestion{}, errors.New("native plan question differs from its committed execution")
	}
	conversation := tracked.Channel
	exchange := ""
	if console.IsConsole(conversation) {
		if strings.HasPrefix(tracked.AnchorMessage, console.AnchorMark) {
			exchange = strings.TrimPrefix(tracked.AnchorMessage, console.AnchorMark)
		}
	} else {
		conversation, err = cons.EnsureRecoveryConversation(ctx, console.RecoveryConversation{ParentTaskID: tracked.ID, SourceChannel: "feishu", SourceConversation: tracked.Channel, Project: tracked.ProjectID})
		if err != nil {
			return consoleapi.PendingQuestion{}, err
		}
	}
	return consoleapi.PendingQuestion{Conversation: conversation, ExchangeID: exchange, Project: record.Project, TaskID: record.TaskID, AttemptID: record.ID, SessionID: nativeSession, RequestID: nativeRequest}, nil
}

func nativePlanQuestions(cons *console.Service, tasks *task.Store, attempts *attempt.Service) (permission.AskFunc, acphost.AskUserFunc) {
	ask := func(ctx context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
		binding, err := planQuestionBinding(ctx, cons, tasks, attempts, q.SessionID, q.RequestID)
		if err != nil {
			return permission.Choose(false, q.Options), err
		}
		return cons.RequestNativePermission(ctx, binding, q)
	}
	askUser := func(ctx context.Context, q view.Question) (view.Answer, error) {
		binding, err := planQuestionBinding(ctx, cons, tasks, attempts, q.SessionID, q.RequestID)
		if err != nil {
			return view.Answer{}, err
		}
		return cons.RequestNativeQuestion(ctx, binding, q)
	}
	return ask, askUser
}
