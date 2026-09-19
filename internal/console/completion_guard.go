package console

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx checks durable questions and delivery in the caller's
// transaction. Only the completion command's own exchange can be exempted.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error {
	raw, _, err := tx.LoadDocument("console")
	if err != nil {
		return fmt.Errorf("read completion console: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var saved transcript
	if err := json.Unmarshal(raw, &saved); err != nil {
		return fmt.Errorf("decode completion console: %w", err)
	}
	for _, question := range saved.Questions {
		if question.State == "pending" && (ids[question.TaskID] || question.Conversation == conversation) {
			return task.ErrCompleteAttention
		}
	}
	for _, exchanges := range saved.Exchanges {
		for _, exchange := range exchanges {
			if exchange == nil {
				return fmt.Errorf("completion console contains a null exchange")
			}
			if ids[exchange.ExpectedTask] && !exchange.State.Terminal() {
				return task.ErrCompleteDelivery
			}
			if exchange.Conversation == conversation && (exchange.State == consoleapi.ExchangeAwaitingUser || exchange.State == consoleapi.ExchangeRecovering) {
				return task.ErrCompleteAttention
			}
			if exchange.Conversation == conversation && !exchange.State.Terminal() {
				if currentExchange == "" || exchange.ID != currentExchange {
					return task.ErrCompleteDelivery
				}
			}
		}
	}
	return nil
}
