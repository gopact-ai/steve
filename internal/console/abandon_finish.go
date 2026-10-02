package console

import (
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// finishAbandonmentLocked commits the receipt, reply and terminal input before
// publishing or releasing any waiter. A refusal restores only memory: the
// durable abandonment remains queued for another reconciliation pass.
func (s *Service) finishAbandonmentLocked(target *queuedExchange, receipt consoleapi.Reply) error {
	if target.RecoveryAbandon != nil && target.RecoveryAbandon.AttemptID != receipt.AttemptID {
		return errors.New("abandonment receipt belongs to another execution")
	}
	previous := *target
	priorReplies := slices.Clone(s.replies[target.Conversation])
	priorExchanges := slices.Clone(s.exchanges[target.Conversation])
	priorQuestions := maps.Clone(s.questions)
	var interrupted []consoleapi.PendingQuestion
	for id, q := range s.questions {
		if q.ExchangeID == target.ID && q.State == "pending" {
			q.State, q.UpdatedAt = "interrupted", time.Now().UTC()
			s.questions[id] = q
			interrupted = append(interrupted, q)
		}
	}
	receipt.At, receipt.Kind = time.Now().UTC(), "reply"
	receipt.Conversation, receipt.ExchangeID = target.Conversation, target.ID
	receipt.Silent = s.stopControl(target.Input)
	if receipt.ProjectID == "" {
		receipt.ProjectID = target.ExpectedProject
	}
	if work := s.processes[target.ID]; work != nil {
		receipt.Process = work.summary()
	}
	receipt = s.recordLocked(receipt)
	target.RecoveryAbandon, target.Receipt = &receipt, &receipt
	target.ReplyID, target.State = receipt.ID, consoleapi.ExchangeCancelled
	target.outcome = outcome{reply: receipt}
	s.trimExchangesLocked(target.Conversation)
	if err := s.save(); err != nil {
		*target = previous
		s.replies[target.Conversation], s.exchanges[target.Conversation] = priorReplies, priorExchanges
		s.questions = priorQuestions
		return err
	}
	s.publishAbandonmentLocked(target, receipt, interrupted)
	return nil
}

func (s *Service) publishAbandonmentLocked(target *queuedExchange, receipt consoleapi.Reply, interrupted []consoleapi.PendingQuestion) {
	delete(s.processes, target.ID)
	if target.cancel != nil {
		target.cancel()
	}
	target.ctx, target.cancel = nil, nil
	if target.done != nil {
		select {
		case <-target.done:
		default:
			if s.running[target.Conversation] > 0 {
				s.running[target.Conversation]--
			}
			close(target.done)
		}
	}
	for _, q := range interrupted {
		if waiter := s.questionWaiters[q.ID]; waiter != nil {
			close(waiter)
			delete(s.questionWaiters, q.ID)
		}
		s.publishQuestion(q)
	}
	s.publishReply(receipt)
	s.publishQueue(target.Conversation)
	if err := s.startNextLocked(target.Conversation); err != nil {
		slog.Warn("console: abandonment delivered; queued input still awaits persistence", "conversation", target.Conversation, "error", err)
	}
}
