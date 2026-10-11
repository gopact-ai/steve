package app

import (
	"context"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/schedule"
)

type scheduledConsole interface {
	EnqueueScheduled(context.Context, schedule.Firing) (console.Exchange, error)
}
type scheduledGateway interface {
	FireSchedule(context.Context, gateway.Fire) (gateway.FireReceipt, error)
}

type consoleScheduleReceiver struct{ page scheduledConsole }

func (r consoleScheduleReceiver) ReceiveSchedule(ctx context.Context, f schedule.Firing) (string, error) {
	// Console acceptance is durable admission. Execution can finish later.
	exchange, err := r.page.EnqueueScheduled(ctx, f)
	return exchange.ID, err
}

type gatewayScheduleReceiver struct{ chat scheduledGateway }

func (r gatewayScheduleReceiver) ReceiveSchedule(ctx context.Context, f schedule.Firing) (string, error) {
	// Gateway returns its announcement receipt after handling the input.
	accepted, err := r.chat.FireSchedule(ctx, gateway.Fire{
		Channel: f.Channel, ProjectID: f.ProjectID, ScheduleID: f.ID, ConversationID: f.ConversationID,
		ChatID: f.ChatID, ChatType: f.ChatType, MessageID: f.AnchorMessage, Requester: f.Requester, Member: f.Member, Prompt: f.Prompt,
	})
	return accepted.MessageID, err
}
