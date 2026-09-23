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
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// planQuestionBinding binds a plan step's or auxiliary execution's question
// to the execution that asked. A node-owned session proves itself through
// its node callback; a hub-local one runs inside the hub's own execution
// scope, whose attempt record is the authority. local reports which.
func planQuestionBinding(ctx context.Context, cons *console.Service, tasks *task.Store, attempts *attempt.Service, sessionID, requestID string) (binding consoleapi.PendingQuestion, local bool, err error) {
	source, nativeSession, nativeRequest, native := harness.NativeQuestionSource(ctx)
	if !native {
		return localPlanQuestionBinding(ctx, cons, tasks, attempts, sessionID)
	}
	if sessionID != nativeSession || requestID != nativeRequest {
		return consoleapi.PendingQuestion{}, false, consoleapi.ErrInvalidAnswer
	}
	record, err := attempts.Get(ctx, source.AttemptID)
	if err != nil {
		return consoleapi.PendingQuestion{}, false, err
	}
	tracked, ok := tasks.Get(source.TaskID)
	if !ok || tracked.ProjectID != source.ProjectID || record.TaskID != source.TaskID || record.Project != source.ProjectID || record.Session != nativeSession || record.Node != source.NodeID || attempt.SessionExecutionEpoch(record) != source.ExecutionEpoch {
		return consoleapi.PendingQuestion{}, false, errors.New("native plan question differs from its committed execution")
	}
	conversation := tracked.Channel
	exchange := ""
	if console.IsConsole(conversation) {
		if strings.HasPrefix(tracked.AnchorMessage, console.AnchorMark) {
			exchange = strings.TrimPrefix(tracked.AnchorMessage, console.AnchorMark)
		}
	} else if conversation, err = recoveryConversationOf(ctx, cons, tracked); err != nil {
		return consoleapi.PendingQuestion{}, false, err
	}
	return consoleapi.PendingQuestion{Conversation: conversation, ExchangeID: exchange, Project: record.Project, TaskID: record.TaskID, AttemptID: record.ID, SessionID: nativeSession, RequestID: nativeRequest}, false, nil
}

// localPlanQuestionBinding reads the asker from the execution scope the
// hub opened for it. The question belongs to the execution, not to the
// exchange that started the plan: the step can outlive that turn.
func localPlanQuestionBinding(ctx context.Context, cons *console.Service, tasks *task.Store, attempts *attempt.Service, sessionID string) (consoleapi.PendingQuestion, bool, error) {
	// A running scope, not a read-only probe key, vouches for the asker.
	key, scoped := execution.KeyOf(ctx)
	if !scoped || execution.Token(ctx) == nil || key.AttemptID == "" || sessionID == "" || lifecycle.IsManaged(sessionID) {
		return consoleapi.PendingQuestion{}, true, consoleapi.ErrInvalidAnswer
	}
	if err := execution.CheckExecution(ctx); err != nil {
		return consoleapi.PendingQuestion{}, true, err
	}
	record, err := attempts.Get(ctx, key.AttemptID)
	if err != nil {
		return consoleapi.PendingQuestion{}, true, err
	}
	tracked, ok := tasks.Get(record.TaskID)
	if !ok || record.TaskID != key.TaskID || !planWork(record.Kind) || record.State.Terminal() || lifecycle.IsManaged(record.Session) || tracked.ProjectID != record.Project {
		return consoleapi.PendingQuestion{}, true, errors.New("local plan question differs from its execution")
	}
	conversation := tracked.Channel
	if !console.IsConsole(conversation) {
		if conversation, err = recoveryConversationOf(ctx, cons, tracked); err != nil {
			return consoleapi.PendingQuestion{}, true, err
		}
	}
	// Like a delegated child's, the question names the recorded session
	// once there is one, not the agent process's own id.
	if record.Session != "" {
		sessionID = record.Session
	}
	return consoleapi.PendingQuestion{Conversation: conversation, Project: record.Project, TaskID: record.TaskID, AttemptID: record.ID, SessionID: sessionID}, true, nil
}

// planWork is what asks through the plan path: steps, their checks and
// planning itself. Turns and delegated children have their own.
func planWork(kind attempt.Kind) bool {
	return kind == attempt.KindStep || kind == attempt.KindVerify || kind == attempt.KindPlan
}

func recoveryConversationOf(ctx context.Context, cons *console.Service, tracked task.Task) (string, error) {
	return cons.EnsureRecoveryConversation(ctx, console.RecoveryConversation{ParentTaskID: tracked.ID, SourceChannel: "feishu", SourceConversation: tracked.Channel, Project: tracked.ProjectID})
}

// planQuestions answers plan steps and auxiliary executions, on a node or
// on the hub.
func planQuestions(cons *console.Service, tasks *task.Store, attempts *attempt.Service) (permission.AskFunc, acphost.AskUserFunc) {
	ask := func(ctx context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
		binding, local, err := planQuestionBinding(ctx, cons, tasks, attempts, q.SessionID, q.RequestID)
		if err != nil {
			return permission.Choose(false, q.Options), err
		}
		if local {
			return cons.RequestLocalPermission(ctx, binding, q)
		}
		return cons.RequestNativePermission(ctx, binding, q)
	}
	askUser := func(ctx context.Context, q view.Question) (view.Answer, error) {
		binding, local, err := planQuestionBinding(ctx, cons, tasks, attempts, q.SessionID, q.RequestID)
		if err != nil {
			return view.Answer{}, err
		}
		if local {
			return cons.RequestLocalQuestion(ctx, binding, q)
		}
		return cons.RequestNativeQuestion(ctx, binding, q)
	}
	return ask, askUser
}
