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
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func delegateQuestionBinding(ctx context.Context, cons *console.Service, binding delegate.QuestionBinding, requestID string) (consoleapi.PendingQuestion, error) {
	conversation := binding.Conversation
	if !console.IsConsole(conversation) {
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
func wireDelegateQuestions(delegation *delegate.Service, cons *console.Service) {
	delegation.SetQuestionHandlers(
		func(ctx context.Context, binding delegate.QuestionBinding, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			pending, err := delegateQuestionBinding(ctx, cons, binding, ask.RequestID)
			if err != nil {
				return permission.Choose(false, ask.Options), err
			}
			if !nodewire.IsManagedSession(binding.Session) {
				return cons.RequestLocalPermission(ctx, pending, ask)
			}
			return cons.RequestNativePermission(ctx, pending, ask)
		},
		func(ctx context.Context, binding delegate.QuestionBinding, question view.Question) (view.Answer, error) {
			pending, err := delegateQuestionBinding(ctx, cons, binding, question.RequestID)
			if err != nil {
				return view.Answer{}, err
			}
			if !nodewire.IsManagedSession(binding.Session) {
				return cons.RequestLocalQuestion(ctx, pending, question)
			}
			return cons.RequestNativeQuestion(ctx, pending, question)
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
func wireDelegateDelivery(delegation *delegate.Service, cons *console.Service, gw *gateway.Gateway) {
	delegation.SetReplaySafeDelivery(func(parent task.Task) bool {
		return parent.Transport == "console"
	})
	delegation.SetDeliveryReceipt(func(parent task.Task, key string) (bool, error) {
		if parent.Transport == "console" {
			return cons.ContinuationReceipt(parent.Channel, parent.ID, key)
		}
		return false, nil
	})
	delegation.SetDeliverer(func(ctx context.Context, d delegate.Delivery) error {
		return routeTask(d.Transport, func() error {
			return cons.ContinueTask(ctx, d.Conversation, d.ParentTask, d.Key, d.Member, d.Notice(), d.Prompt())
		}, func() error {
			return gw.DeliverConfirmed(gateway.Revival{TaskID: d.ParentTask, Member: d.Member, ConversationID: d.Conversation, ChatID: d.ChatID, MessageID: d.Anchor, Requester: d.Requester, ChatType: d.ChatType}, d.Notice(), d.Prompt(), func(err error) { delegation.ConfirmDelivery(d, err) })
		})
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
