package console

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/protocol"
)

type recoveryStopTarget struct {
	Conversation string `json:"conversation"`
	ExchangeID   string `json:"exchange_id"`
	TaskID       string `json:"task_id,omitempty"`
	Requester    string `json:"requester"`
}

// A failed stop is an acknowledged attempt to reach a fixed task, not proof
// that it stopped. An explicit retry may refresh that same operation's receipt.
func (s *Service) retryRecoveryStopLocked(e *queuedExchange) (*queuedExchange, error) {
	index := -1
	for i, current := range s.exchanges[e.Conversation] {
		if current.ID == e.ID {
			e, index = current, i
			break
		}
	}
	if index < 0 {
		return nil, consoleapi.ErrExchangeNotFound
	}
	if e.State != "failed" || e.Key == "" {
		return e, nil
	}
	address, parsed := s.parseInput(e.Input)
	if address != "" || parsed.Command != protocol.CommandCancel {
		return e, nil
	}
	if e.RecoveryStopTarget == nil {
		// Older stop receipts have no target field. Their exact pending error on
		// one earlier exchange is the only accepted association; never select the
		// current task merely because this old control said /cancel.
		if e.Receipt == nil || e.Receipt.Error == "" {
			return e, nil
		}
		var found *queuedExchange
		foundTask := ""
		for _, candidate := range s.exchanges[e.Conversation] {
			if candidate.ID == e.ID || candidate.RecoveryStopPending != e.Receipt.Error || candidate.EnqueuedAt.After(e.EnqueuedAt) {
				continue
			}
			matchedAttempt := false
			for _, question := range s.questions {
				if question.Conversation == e.Conversation && question.ExchangeID == candidate.ID && question.AttemptID != "" && question.TaskID != "" && question.Principal == s.owner && strings.Contains(e.Receipt.Error, "attempt "+question.AttemptID+" ") {
					matchedAttempt = true
					foundTask = question.TaskID
					break
				}
			}
			if !matchedAttempt {
				continue
			}
			if found != nil {
				return nil, errors.New("the original stop target is ambiguous; issue a new stop for the waiting task")
			}
			found = candidate
		}
		if found == nil {
			return e, nil
		}
		e.RecoveryStopTarget = &recoveryStopTarget{Conversation: e.Conversation, ExchangeID: found.ID, TaskID: foundTask, Requester: s.owner}
	}
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		return nil, consoleapi.ErrConsoleClosing
	}
	old := e
	next := *e
	e = &next
	s.exchanges[e.Conversation][index] = e
	ctx, cancel := context.WithCancel(s.exchangeContext(context.Background()))
	e.State, e.ctx, e.cancel, e.done = "running", ctx, cancel, make(chan struct{})
	s.running[e.Conversation]++
	if err := s.save(); err != nil {
		s.exchanges[e.Conversation][index] = old
		s.running[e.Conversation]--
		cancel()
		return nil, err
	}
	s.publishQueue(e.Conversation)
	exchange := copyExchange(e.Exchange)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		reply, err := s.runExchange(ctx, exchange)
		s.finish(e, reply, err)
	}()
	return e, nil
}

// Stop targets are fixed in the control's first durable admission. A crash
// before the handler or a failed lookup cannot turn a retry into a new target.
func (s *Service) bindRecoveryStopTargetLocked(control *queuedExchange) {
	if control.RecoveryStopTarget != nil {
		return
	}
	address, parsed := s.parseInput(control.Input)
	if address != "" || parsed.Command != protocol.CommandCancel {
		return
	}
	var target *queuedExchange
	for _, e := range s.exchanges[control.Conversation] {
		if e.ID == control.ID || (e.State != "recovering" && e.State != "awaiting-user") {
			continue
		}
		if target != nil {
			return
		}
		target = e
	}
	if target != nil && (target.Requester == "" || target.Requester == s.owner) {
		control.RecoveryStopTarget = &recoveryStopTarget{Conversation: control.Conversation, ExchangeID: target.ID, Requester: s.owner}
	}
}
