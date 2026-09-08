package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
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
type scheduledCoordinator interface {
	RotateTask(conversation, member, origin string)
}

// runScheduleDispatcher uses a durable firing as the handoff between the
// clock and the channel's receiver. Sending is never the same as claiming.
func runScheduleDispatcher(ctx context.Context, store *schedule.Store, page scheduledConsole, chat scheduledGateway, coordinator scheduledCoordinator) {
	dispatch := func(now time.Time) {
		due, err := store.Due(now)
		if err != nil {
			log.Printf("schedule: find due: %v", err)
			return
		}
		for _, f := range due {
			if err := store.BeginFiring(f.Key); err != nil {
				continue
			}
			go dispatchFiring(ctx, store, page, chat, coordinator, f)
		}
	}
	dispatch(time.Now())
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			dispatch(now)
		}
	}
}

func dispatchFiring(ctx context.Context, store *schedule.Store, page scheduledConsole, chat scheduledGateway, coordinator scheduledCoordinator, f schedule.Firing) {
	if coordinator != nil {
		coordinator.RotateTask(f.ConversationID, f.Member, "schedule:"+f.ID)
	}
	var receipt string
	var err error
	switch f.Channel {
	case "console":
		if page == nil {
			err = errors.New("console channel is not available")
			break
		}
		var exchange console.Exchange
		exchange, err = page.EnqueueScheduled(ctx, f)
		if err == nil {
			receipt = exchange.ID
		}
	case "feishu":
		if chat == nil {
			err = errors.New("Feishu channel is not available")
			break
		}
		var accepted gateway.FireReceipt
		accepted, err = chat.FireSchedule(ctx, gateway.Fire{
			Channel: f.Channel, ProjectID: f.ProjectID, ScheduleID: f.ID, ConversationID: f.ConversationID,
			ChatID: f.ChatID, ChatType: f.ChatType, MessageID: f.AnchorMessage, Requester: f.Requester, Member: f.Member, Prompt: f.Prompt,
		})
		if err == nil {
			receipt = accepted.MessageID
		}
	default:
		err = fmt.Errorf("schedule channel %q is not configured", f.Channel)
	}
	if err == nil && receipt == "" {
		err = fmt.Errorf("%w: empty schedule delivery receipt", channel.ErrOutcomeUnknown)
	}
	if err != nil {
		unknown := errors.Is(err, channel.ErrOutcomeUnknown)
		if saveErr := store.FailFiring(f.Key, err, unknown); saveErr != nil {
			log.Printf("schedule %s: %v; record outcome: %v", f.Key, err, saveErr)
		} else {
			log.Printf("schedule %s: %v", f.Key, err)
		}
		return
	}
	if err := store.AcceptFiring(f.Key, receipt, time.Now()); err != nil {
		log.Printf("schedule %s: delivery accepted as %s, save receipt: %v", f.Key, receipt, err)
	}
}

// scheduleTick is how often standing work is checked. Twenty seconds is fine
// grain for a surface whose shortest interval is a minute, and cheap: due
// jobs are a map scan, and a tick with nothing due writes nothing.
const scheduleTick = 20 * time.Second
