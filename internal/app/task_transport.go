package app

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// taskReceiver owns the adapter's handoff and receipt semantics. ReplaySafe
// means the adapter durably deduplicates the exact continuation key; registering
// a receiver alone does not grant that capability or prove external delivery.
type taskReceiver interface {
	schedule.ScheduleReceiver
	Notice(turn.TaskNotice) error
	Resume(context.Context, turn.TaskResume) error
	DispatchResume(context.Context, turn.TaskResume) error
	Deliver(context.Context, delegate.Delivery, func(error)) error
	ReplaySafe() bool
	ContinuationReceipt(task.Task, string) (bool, error)
}

func (r *taskChannelRegistry) ReceiveSchedule(ctx context.Context, firing schedule.Firing) (string, error) {
	receiver, err := r.receiver(firing.Channel)
	if err != nil {
		return "", err
	}
	return receiver.ReceiveSchedule(ctx, firing)
}

// taskChannelRegistry is fixed before services start. All task handoffs select
// only a persisted transport; opaque conversation/message IDs never select it.
type taskChannelRegistry struct{ receivers map[string]taskReceiver }

func newTaskChannels(receivers map[string]taskReceiver) (*taskChannelRegistry, error) {
	for name, receiver := range receivers {
		if name == "" || strings.TrimSpace(name) != name || receiver == nil {
			return nil, fmt.Errorf("task channel %q needs a name and receiver", name)
		}
	}
	return &taskChannelRegistry{receivers: maps.Clone(receivers)}, nil
}

func (r *taskChannelRegistry) receiver(transport string) (taskReceiver, error) {
	if receiver, ok := r.receivers[transport]; ok {
		return receiver, nil
	}
	return nil, fmt.Errorf("unsupported task transport %q", transport)
}

func (r *taskChannelRegistry) Notice(n turn.TaskNotice) error {
	receiver, err := r.receiver(n.Transport)
	if err != nil {
		return err
	}
	return receiver.Notice(n)
}

func (r *taskChannelRegistry) Resume(ctx context.Context, input turn.TaskResume) error {
	receiver, err := r.receiver(input.Transport)
	if err != nil {
		return err
	}
	return receiver.Resume(ctx, input)
}

func (r *taskChannelRegistry) DispatchResume(ctx context.Context, input turn.TaskResume) error {
	receiver, err := r.receiver(input.Transport)
	if err != nil {
		return err
	}
	return receiver.DispatchResume(ctx, input)
}

func (r *taskChannelRegistry) Deliver(ctx context.Context, input delegate.Delivery, confirm func(error)) error {
	receiver, err := r.receiver(input.Transport)
	if err != nil {
		return err
	}
	return receiver.Deliver(ctx, input, confirm)
}

func (r *taskChannelRegistry) ReplaySafe(parent task.Task) bool {
	receiver, err := r.receiver(parent.Transport)
	return err == nil && receiver.ReplaySafe()
}

func (r *taskChannelRegistry) ContinuationReceipt(parent task.Task, key string) (bool, error) {
	receiver, err := r.receiver(parent.Transport)
	if err != nil {
		return false, nil // no receipt evidence for an unknown transport
	}
	return receiver.ContinuationReceipt(parent, key)
}
