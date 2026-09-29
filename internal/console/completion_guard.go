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
		if question.State == "pending" && (ids[question.TaskID] || question.Conversation == conversation) {
			return task.ErrCompleteAttention
		}
	}
	for _, list := range state.Exchanges {
		for _, exchange := range list {
			if ids[exchange.ExpectedTask] && !exchange.State.Terminal() {
				return task.ErrCompleteDelivery
			}
			if exchange.Conversation == conversation && (exchange.State == consoleapi.ExchangeAwaitingUser || exchange.State == consoleapi.ExchangeRecovering) {
				return task.ErrCompleteAttention
			}
			if exchange.Conversation == conversation && !exchange.State.Terminal() {
				if spareQueued && exchange.State == consoleapi.ExchangeQueued && exchange.ExpectedTask == "" {
					continue
				}
				if currentExchange == "" || exchange.ID != currentExchange {
					return task.ErrCompleteDelivery
				}
			}
		}
	}
	return nil
}
