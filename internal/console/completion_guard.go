package console

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx checks durable questions and delivery in the caller's
// transaction. The exchange of the command asking for the end,
// currentExchange, is exempted. With spareQueued, so is every line of the
// conversation still queued that no task has claimed — input typed behind
// the command, a scheduled prompt: it has not started, and runs after the
// end under whichever task then holds the conversation.
//
// What the tasks themselves hold is looked for first: a question one of
// them asked, a line one of them expects. The rest of what is pending in
// the conversation — a question or a line of another task, or of none — is
// refused with task.ErrCompleteConversation, since ending or cancelling
// these tasks does not settle it.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string, spareQueued bool) error {
	records, err := loadConsoleRecordsTx(tx)
	if err != nil {
		return fmt.Errorf("read completion console: %w", err)
	}
	state, err := records.state()
	if err != nil {
		return fmt.Errorf("read completion console: %w", err)
	}
	for _, question := range state.Questions {
		if question.State == "pending" && ids[question.TaskID] {
			return task.ErrCompleteAttention
		}
	}
	for _, list := range state.Exchanges {
		for _, exchange := range list {
			if ids[exchange.ExpectedTask] && !exchange.State.Terminal() {
				return task.ErrCompleteDelivery
			}
		}
	}
	for _, question := range state.Questions {
		if question.State == "pending" && question.Conversation == conversation {
			return fmt.Errorf("%w: question %s: %w", task.ErrCompleteAttention, question.ID, task.ErrCompleteConversation)
		}
	}
	for _, list := range state.Exchanges {
		for _, exchange := range list {
			if exchange.Conversation != conversation || exchange.State.Terminal() {
				continue
			}
			if exchange.State == consoleapi.ExchangeAwaitingUser || exchange.State == consoleapi.ExchangeRecovering {
				return fmt.Errorf("%w: exchange %s (%s): %w", task.ErrCompleteAttention, exchange.ID, exchange.State, task.ErrCompleteConversation)
			}
			if spareQueued && exchange.State == consoleapi.ExchangeQueued && exchange.ExpectedTask == "" {
				continue
			}
			if currentExchange == "" || exchange.ID != currentExchange {
				return fmt.Errorf("%w: exchange %s (%s): %w", task.ErrCompleteDelivery, exchange.ID, exchange.State, task.ErrCompleteConversation)
			}
		}
	}
	return nil
}
