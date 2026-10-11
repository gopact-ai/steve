package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/interaction"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func delegateQuestionBinding(ctx context.Context, cons *console.Service, binding delegate.QuestionBinding, requestID string) (consoleapi.PendingQuestion, error) {
	conversation := binding.Conversation
	if binding.Transport != "console" {
		var err error
		conversation, err = cons.EnsureRecoveryConversation(ctx, console.RecoveryConversation{ParentTaskID: binding.ParentTask, SourceChannel: sourceChannel(binding.Transport), SourceConversation: binding.Conversation, Project: binding.Project})
		if err != nil {
			return consoleapi.PendingQuestion{}, err
		}
	}
	if !nodewire.IsManagedSession(binding.Session) {
		requestID = ""
	}
	return consoleapi.PendingQuestion{Conversation: conversation, Project: binding.Project, TaskID: binding.Task, ParentTaskID: binding.ParentTask, AttemptID: binding.Attempt, SessionID: binding.Session, RequestID: requestID}, nil
}

// wireDelegateQuestions puts a child's questions before the owner in its
// parent's conversation. A node-owned child answers to its node callback;
// a hub-local child to the hub's own execution record, in any deployment.
func wireDelegateQuestions(delegation *delegate.Service, cons *console.Service, services ...*interaction.Service) {
	var shared *interaction.Service
	if len(services) > 0 {
		shared = services[0]
	}
	delegation.SetQuestionHandlers(
		func(ctx context.Context, binding delegate.QuestionBinding, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			pending, err := delegateQuestionBinding(ctx, cons, binding, ask.RequestID)
			if err != nil {
				return permission.Choose(false, ask.Options), err
			}
			out, err := shared.RequestExecution(ctx, binding.Attempt, "permission", ask, func() (any, error) {
				if !nodewire.IsManagedSession(binding.Session) {
					return cons.RequestLocalPermission(ctx, pending, ask)
				}
				return cons.RequestNativePermission(ctx, pending, ask)
			})
			if err != nil {
				return permission.Choose(false, ask.Options), err
			}
			return out.(acp.RequestPermissionOutcome), nil
		},
		func(ctx context.Context, binding delegate.QuestionBinding, question view.Question) (view.Answer, error) {
			pending, err := delegateQuestionBinding(ctx, cons, binding, question.RequestID)
			if err != nil {
				return view.Answer{}, err
			}
			out, err := shared.RequestExecution(ctx, binding.Attempt, "question", question, func() (any, error) {
				if !nodewire.IsManagedSession(binding.Session) {
					return cons.RequestLocalQuestion(ctx, pending, question)
				}
				return cons.RequestNativeQuestion(ctx, pending, question)
			})
			if err != nil {
				return view.Answer{}, err
			}
			return out.(view.Answer), nil
		},
	)
}

// wireDelegateRecovery tells the owner when a node-owned child cannot be
// joined again.
func wireDelegateRecovery(delegation *delegate.Service, cons *console.Service) {
	delegation.SetRecoveryQuestion(func(ctx context.Context, q delegate.RecoveryQuestion) (view.Answer, error) {
		binding, err := delegateQuestionBinding(ctx, cons, q.QuestionBinding, q.Question.RequestID)
		if err != nil {
			return view.Answer{}, err
		}
		return cons.RequestRecovery(ctx, binding, q.Question)
	})
}

// wireDelegateDelivery sends a child's result back into its parent's
// conversation as a message — the page's queue or the chat — instead of
// the parent polling for it; a turn's end delivers what ended meanwhile.
func wireDelegateDelivery(delegation *delegate.Service, channels *taskChannelRegistry) {
	delegation.SetReplaySafeDelivery(channels.ReplaySafe)
	delegation.SetDeliveryReceipt(channels.ContinuationReceipt)
	delegation.SetDeliverer(func(ctx context.Context, d delegate.Delivery) error {
		return channels.Deliver(ctx, d, func(err error) { delegation.ConfirmDelivery(d, err) })
	})
}

func runReconciler(ctx context.Context, label string, reconcile func(context.Context) error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Error(fmt.Sprintf("steve: %s: %v", label, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
