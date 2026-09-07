package console

import (
	"context"
	"errors"
	"sync"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedStopDriver interface {
	StopRetainedTask(context.Context, string, turn.Request) (turn.Result, error)
}

// A recovery question is owned by the console after the coordinator's turn
// has detached. Stop must reach that original task, not an empty turn slot.
func (s *Service) stopRecovering(ctx context.Context, control Exchange, requester string) (turn.Result, bool, error) {
	s.mu.Lock()
	var controlRecord *queuedExchange
	for _, e := range s.exchanges[control.Conversation] {
		if e.ID == control.ID {
			controlRecord = e
			break
		}
	}
	var bound *recoveryStopTarget
	if controlRecord != nil {
		bound = controlRecord.RecoveryStopTarget
	}
	var target *queuedExchange
	for _, e := range s.exchanges[control.Conversation] {
		if e.ID == control.ID || (bound != nil && e.ID != bound.ExchangeID) || (bound == nil && e.State != "recovering" && e.State != "awaiting-user") {
			continue
		}
		if target != nil {
			s.mu.Unlock()
			return turn.Result{}, true, errors.New("multiple recovering tasks require an explicit task selection")
		}
		target = e
	}
	driver := s.recoveryDriver
	if target == nil {
		s.mu.Unlock()
		if bound != nil {
			return turn.Result{}, true, errors.New("original stop target is unavailable")
		}
		return turn.Result{}, false, nil
	}
	if bound != nil && (bound.Requester != requester || bound.Conversation != control.Conversation) {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("original stop target identity changed")
	}
	if target.RecoveryStop != nil {
		reply := *target.RecoveryStop
		s.mu.Unlock()
		return turn.Result{Text: reply.Text, Title: reply.Title}, true, nil
	}
	if terminalExchange(target.State) {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("original stop target has already finished without a stop receipt")
	}
	if target.Requester != "" && target.Requester != requester {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("recovery requires the original requester")
	}
	if controlRecord != nil && controlRecord.RecoveryStopTarget == nil {
		controlRecord.RecoveryStopTarget = &recoveryStopTarget{Conversation: control.Conversation, ExchangeID: target.ID, Requester: requester}
		if err := s.save(); err != nil {
			controlRecord.RecoveryStopTarget = nil
			s.mu.Unlock()
			return turn.Result{}, true, err
		}
	}
	if target.recoveryStopping != nil {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("the original recovery task is already stopping")
	}
	target.recoveryStopping = make(chan struct{})
	var released sync.Once
	release := func() {
		released.Do(func() {
			s.mu.Lock()
			close(target.recoveryStopping)
			target.recoveryStopping = nil
			s.mu.Unlock()
		})
	}
	defer release()
	original := copyExchange(target.Exchange)
	s.mu.Unlock()
	stopper, ok := driver.(retainedStopDriver)
	if !ok {
		return turn.Result{}, true, errors.New("stopping the original recovery task is unavailable")
	}
	candidate, found, err := s.findRetained(ctx, driver, original)
	if err != nil || !found {
		return turn.Result{}, true, errors.Join(harness.ErrStopUnconfirmed, err)
	}
	if original.Requester != "" && original.Requester != requester {
		return turn.Result{}, true, errors.New("recovery requires the original requester")
	}
	if bound != nil && bound.TaskID != "" && bound.TaskID != candidate.TaskID {
		return turn.Result{}, true, errors.New("original stop task identity changed")
	}
	s.mu.Lock()
	if controlRecord != nil {
		controlRecord.RecoveryStopTarget = &recoveryStopTarget{Conversation: control.Conversation, ExchangeID: target.ID, TaskID: candidate.TaskID, Requester: requester}
	}
	previousPending := target.RecoveryStopPending
	target.RecoveryStopPending = "Stop requested; waiting for confirmation from the original task and its children."
	if err := s.save(); err != nil {
		target.RecoveryStopPending = previousPending
		s.mu.Unlock()
		return turn.Result{}, true, err
	}
	s.mu.Unlock()
	result, err := stopper.StopRetainedTask(ctx, candidate.TaskID, turn.Request{Channel: "console", ConversationID: original.Conversation, MessageID: AnchorMark + original.ID, SenderOpenID: requester, ExpectedProject: original.ExpectedProject, Locale: control.Locale})
	if err != nil {
		s.mu.Lock()
		target.RecoveryStopPending = err.Error()
		saveErr := s.save()
		s.mu.Unlock()
		return result, true, errors.Join(err, saveErr)
	}
	reply := consoleapi.Reply{Text: result.Text, Title: result.Title}
	s.mu.Lock()
	if terminalExchange(target.State) {
		s.mu.Unlock()
		return result, true, nil
	}
	// Persist confirmed settlement before interrupting the waiter. A crash in
	// this gap must finish delivery on restart, never ask or execute again.
	previous, pending := target.RecoveryStop, target.RecoveryStopPending
	target.RecoveryStop, target.RecoveryStopPending = &reply, ""
	if err := s.save(); err != nil {
		target.RecoveryStop, target.RecoveryStopPending = previous, pending
		s.mu.Unlock()
		return turn.Result{}, true, err
	}
	cancel, done := target.cancel, target.done
	if cancel == nil {
		select {
		case <-done:
			// A failed recovery observer already released its reservation.
			// Reserve just the terminal delivery before finish releases it.
			target.done = make(chan struct{})
			done = target.done
			s.running[target.Conversation]++
		default:
		}
	}
	s.mu.Unlock()
	release()
	if cancel != nil {
		cancel()
	} else {
		s.finish(target, reply, nil)
	}
	select {
	case <-done:
		return result, true, nil
	case <-ctx.Done():
		return turn.Result{}, true, ctx.Err()
	}
}

// A parent observer can finish before the stop of one of its children is
// confirmed. Keep the original exchange reserved until the whole task settles.
func (s *Service) waitRecoveryStop(e *queuedExchange) {
	s.mu.Lock()
	if e.RecoveryStop != nil {
		reply := *e.RecoveryStop
		s.mu.Unlock()
		s.finish(e, reply, nil)
		return
	}
	ctx := e.ctx
	e.State = "awaiting-user"
	message := e.RecoveryStopPending
	saveErr := s.save()
	s.publishQueue(e.Conversation)
	base := consoleapi.PendingQuestion{Conversation: e.Conversation, ExchangeID: e.ID, Project: e.ExpectedProject, Locale: e.Locale}
	s.mu.Unlock()
	if saveErr != nil {
		s.detachRecovery(e, saveErr)
		return
	}
	question := view.Question{Kind: "recovery", Title: "等待确认任务已停止", Message: "已记录停止要求并检查原任务及其子任务。仍有执行尚未确认停止：\n\n" + message + "\n\n任务和待发指令已保留。请恢复相关机器或核实原执行，然后再次点击停止以重新检查。", Required: true, Choices: []view.Choice{{Value: "wait", Label: "等待处理"}}}
	if base.Locale == "en" {
		question.Title = "Waiting to confirm the task has stopped"
		question.Message = "The stop request was recorded and the original task and its children were checked. Some executions have not confirmed stopping:\n\n" + message + "\n\nThe task and queued instructions are preserved. Restore the relevant machine or verify the original execution, then click Stop again to recheck."
		question.Choices[0].Label = "Wait for resolution"
	}
	_, err := s.askUser(ctx, base, question)
	if err == nil {
		<-ctx.Done()
		err = ctx.Err()
	}
	s.detachRecovery(e, err)
}
