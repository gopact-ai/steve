package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/schedule"
)

type scheduledCoordinator interface {
	RotateTask(conversation, member, origin string)
}

// runScheduleDispatcher uses a durable firing as the handoff between the
// clock and the channel's receiver. Sending is never the same as claiming.
func runScheduleDispatcher(ctx context.Context, store *schedule.Store, receivers schedule.ScheduleReceiver, coordinator scheduledCoordinator) {
	dispatch := func(now time.Time) {
		due, err := store.Due(now)
		if err != nil {
			slog.Error(fmt.Sprintf("schedule: find due: %v", err))
			return
		}
		for _, f := range due {
			if err := store.BeginFiring(f.Key); err != nil {
				continue
			}
			go dispatchFiring(ctx, store, receivers, coordinator, f)
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

func dispatchFiring(ctx context.Context, store *schedule.Store, receivers schedule.ScheduleReceiver, coordinator scheduledCoordinator, f schedule.Firing) {
	if coordinator != nil {
		coordinator.RotateTask(f.ConversationID, f.Member, "schedule:"+f.ID)
	}
	receipt, err := receivers.ReceiveSchedule(ctx, f)
	if err == nil && receipt == "" {
		err = fmt.Errorf("%w: empty schedule delivery receipt", channel.ErrOutcomeUnknown)
	}
	if err != nil {
		unknown := errors.Is(err, channel.ErrOutcomeUnknown)
		if saveErr := store.FailFiring(f.Key, err, unknown); saveErr != nil {
			slog.Error(fmt.Sprintf("schedule %s: %v; record outcome: %v", f.Key, err, saveErr), "schedule", f.Key)
		} else {
			slog.Error(fmt.Sprintf("schedule %s: %v", f.Key, err), "schedule", f.Key)
		}
		return
	}
	if err := store.AcceptFiring(f.Key, receipt, time.Now()); err != nil {
		slog.Error(fmt.Sprintf("schedule %s: delivery accepted as %s, save receipt: %v", f.Key, receipt, err), "schedule", f.Key)
	}
}

// scheduleTick is how often standing work is checked. Twenty seconds is fine
// grain for a surface whose shortest interval is a minute, and cheap: due
// jobs are a map scan, and a tick with nothing due writes nothing.
const scheduleTick = 20 * time.Second
