package schedule

import (
	"context"
	"fmt"
	"sync"
)

// ScheduleReceiver hands a frozen occurrence to a channel's existing receiver.
// A nonempty receipt identifies that channel's handoff, not a universal promise
// of execution completion or external delivery. Receivers must preserve the
// firing key, requester and task admission semantics of their channel.
// An uncertain handoff must return an error wrapping channel.ErrOutcomeUnknown.
// Registration does not grant replay safety: Store retains its recovery policy.
type ScheduleReceiver interface {
	ReceiveSchedule(context.Context, Firing) (string, error)
}

// ReceiverRegistry explicitly binds channel names to receivers. Its zero value
// is usable. Registrations cannot replace an existing receiver; a firing already
// in progress continues with the receiver it resolved.
type ReceiverRegistry struct {
	mu        sync.RWMutex
	receivers map[string]ScheduleReceiver
}

func (r *ReceiverRegistry) Register(channel string, receiver ScheduleReceiver) error {
	if channel == "" || receiver == nil {
		return fmt.Errorf("schedule receiver requires a channel and receiver")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.receivers[channel] != nil {
		return fmt.Errorf("schedule channel %q is already registered", channel)
	}
	if r.receivers == nil {
		r.receivers = make(map[string]ScheduleReceiver)
	}
	r.receivers[channel] = receiver
	return nil
}

func (r *ReceiverRegistry) ReceiveSchedule(ctx context.Context, f Firing) (string, error) {
	r.mu.RLock()
	receiver := r.receivers[f.Channel]
	r.mu.RUnlock()
	if receiver == nil {
		return "", fmt.Errorf("schedule channel %q is not configured", f.Channel)
	}
	return receiver.ReceiveSchedule(ctx, f)
}
