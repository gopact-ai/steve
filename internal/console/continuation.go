package console

import (
	"context"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// ContinueTask keeps the accepted message bound to the parent across queueing
// and restarts. Durable ingress and confirmed parent processing are distinct.
func (s *Service) ContinueTask(ctx context.Context, conversation, taskID, key, member, notice, prompt string) error {
	if member == "" || taskID == "" || key == "" {
		return fmt.Errorf("task continuation needs a parent and member")
	}
	// Once accepted, the durable input is authoritative. Landing descriptions
	// may change while its receipt is pending; never rebuild that message.
	s.mu.Lock()
	e := s.continuationLocked(conversation, key)
	s.mu.Unlock()
	var err error
	if e == nil {
		e, _, err = s.enqueue(ctx, conversation, notice, nil, enqueueOptions{Prompt: "@" + member + " " + prompt, Front: true, Key: key, ExpectedTask: taskID})
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ExpectedTask != taskID {
		return fmt.Errorf("%w: continuation belongs to another task", channel.ErrOutcomeUnknown)
	}
	if e.State != consoleapi.ExchangeQueued && !continuationSettled(e) {
		return channel.ErrDeliveryQueued
	}
	if e.State == consoleapi.ExchangeFailed && e.ContinuationRejected {
		if err := s.requeueContinuationLocked(ctx, e); err != nil {
			return err
		}
	}
	switch e.State {
	case consoleapi.ExchangeDone:
		return nil
	case consoleapi.ExchangeQueued:
		if s.running[e.Conversation] == 0 {
			if err := s.startNextLocked(e.Conversation); err != nil {
				return err
			}
		}
		return channel.ErrDeliveryQueued
	case consoleapi.ExchangeRunning, consoleapi.ExchangeRecovering:
		return channel.ErrDeliveryQueued
	default:
		return fmt.Errorf("%w: parent exchange %s is %s; inspect its retained reply", channel.ErrOutcomeUnknown, e.ID, e.State)
	}
}

// ContinuationReceipt inspects durable processing evidence without enqueueing
// or resuming anything. A pre-admission rejection has no processing receipt.
func (s *Service) ContinuationReceipt(conversation, taskID, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.continuationLocked(conversation, key)
	if e == nil {
		return false, nil
	}
	if e.ExpectedTask != taskID {
		return true, fmt.Errorf("%w: continuation belongs to another task", channel.ErrOutcomeUnknown)
	}
	if e.State == consoleapi.ExchangeQueued {
		// A previous start may have failed before saving the reservation.
		// Let normal admission retry that same queued entry when permitted.
		return false, nil
	}
	if !continuationSettled(e) {
		return true, channel.ErrDeliveryQueued
	}
	if e.State == consoleapi.ExchangeDone {
		return true, nil
	}
	if e.State == consoleapi.ExchangeFailed && e.ContinuationRejected {
		return false, nil
	}
	return true, fmt.Errorf("%w: parent exchange %s is %s; inspect its retained reply", channel.ErrOutcomeUnknown, e.ID, e.State)
}

func (s *Service) continuationLocked(conversation, key string) *queuedExchange {
	for _, e := range s.exchanges[conversationID(conversation)] {
		if key != "" && e.Key == key {
			return e
		}
	}
	return nil
}

// finish can temporarily release the mutex while retrying persistence. The
// terminal state is a receipt only after its completion channel is closed.
func continuationSettled(e *queuedExchange) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Only a persisted rejection before task admission may requeue the same input.
// A failed or cancelled admitted execution is never automatically run twice.
func (s *Service) requeueContinuationLocked(ctx context.Context, e *queuedExchange) error {
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		return consoleapi.ErrConsoleClosing
	}
	previous := *e
	e.State, e.StartedAt, e.ReplyID = consoleapi.ExchangeQueued, time.Time{}, ""
	e.Receipt, e.ContinuationRejected = nil, false
	e.ctx, e.cancel, e.done = s.exchangeContext(ctx), nil, make(chan struct{})
	e.outcome = outcome{}
	if err := s.save(); err != nil {
		*e = previous
		return err
	}
	s.publishQueue(e.Conversation)
	return nil
}
