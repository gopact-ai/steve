package app

import (
	"context"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// assembleTaskChannels is the sole registration point for task handoffs. A new
// adapter implements taskReceiver; routing and delegation need no channel switch.
func assembleTaskChannels(page *console.Service, chat *gateway.Gateway, book *ledger.Ledger, coordinator *turn.Coordinator, text i18n.Catalog, native ...map[string]*gateway.Gateway) (*taskChannelRegistry, error) {
	registered := map[string]taskReceiver{
		"console": consoleTaskReceiver{page: page, revive: coordinator.ReviveSession, text: text},
		"feishu":  gatewayTaskReceiver{chat: chat, book: book, driver: coordinator, revive: coordinator.ReviveSession},
	}
	for _, channels := range native {
		for name, instance := range channels {
			registered[name] = gatewayTaskReceiver{chat: instance, book: book, driver: coordinator, revive: coordinator.ReviveSession}
		}
	}
	return newTaskChannels(registered)
}

type consoleTaskReceiver struct {
	page   *console.Service
	revive func(string, string) error
	text   i18n.Catalog
}

func (r consoleTaskReceiver) ReceiveSchedule(ctx context.Context, firing schedule.Firing) (string, error) {
	return (consoleScheduleReceiver{page: r.page}).ReceiveSchedule(ctx, firing)
}

func (r consoleTaskReceiver) Notice(n turn.TaskNotice) error {
	r.page.Notice(n)
	return nil
}

func (r consoleTaskReceiver) Resume(ctx context.Context, input turn.TaskResume) error {
	return r.page.Resume(ctx, input.ConversationID, input.TaskID, input.Member,
		r.text.T(i18n.TaskResumeNotice, input.TaskID), r.text.T(i18n.TaskResumeManual, input.Goal), input.Admission, r.revive)
}

func (r consoleTaskReceiver) DispatchResume(_ context.Context, input turn.TaskResume) error {
	return r.page.DispatchResume(input.ConversationID, input.Admission)
}

func (r consoleTaskReceiver) Deliver(ctx context.Context, input delegate.Delivery, _ func(error)) error {
	return r.page.ContinueTask(ctx, input.Conversation, input.ParentTask, input.Key, input.Member, input.Notice(), input.Prompt())
}

func (consoleTaskReceiver) ReplaySafe() bool { return true }

func (r consoleTaskReceiver) ContinuationReceipt(parent task.Task, key string) (bool, error) {
	return r.page.ContinuationReceipt(parent.Channel, parent.ID, key)
}

type gatewayTaskReceiver struct {
	chat   *gateway.Gateway
	book   *ledger.Ledger
	driver gateway.RecoveryDriver
	revive func(string, string) error
}

func (r gatewayTaskReceiver) ReceiveSchedule(ctx context.Context, firing schedule.Firing) (string, error) {
	return (gatewayScheduleReceiver{chat: r.chat}).ReceiveSchedule(ctx, firing)
}

func (r gatewayTaskReceiver) Notice(n turn.TaskNotice) error {
	r.chat.Notify(gateway.Notice{TaskID: n.TaskID, MessageID: n.MessageID, Requester: n.Requester, Text: n.Text})
	return nil
}

func (r gatewayTaskReceiver) Resume(ctx context.Context, input turn.TaskResume) error {
	return r.chat.QueueTaskResume(ctx, r.book, input.Admission.ID, gateway.Revival{
		TaskID: input.TaskID, Goal: input.Goal, Member: input.Member, ConversationID: input.ConversationID,
		ChatID: input.ChatID, MessageID: input.MessageID, Requester: input.Requester, ChatType: input.ChatType, Manual: true,
	}, input.Admission)
}

func (r gatewayTaskReceiver) DispatchResume(ctx context.Context, input turn.TaskResume) error {
	return r.chat.DispatchResume(ctx, r.book, input.Admission, r.driver, r.revive)
}

func (r gatewayTaskReceiver) Deliver(_ context.Context, input delegate.Delivery, confirm func(error)) error {
	return r.chat.DeliverConfirmed(gateway.Revival{
		TaskID: input.ParentTask, Member: input.Member, ConversationID: input.Conversation,
		ChatID: input.ChatID, MessageID: input.Anchor, Requester: input.Requester, ChatType: input.ChatType,
	}, input.Notice(), input.Prompt(), confirm)
}

func (gatewayTaskReceiver) ReplaySafe() bool { return false }

func (gatewayTaskReceiver) ContinuationReceipt(task.Task, string) (bool, error) {
	return false, nil
}
