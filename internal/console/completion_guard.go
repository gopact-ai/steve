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
	return checkTaskCompletion(tx, state, ids, conversation, currentExchange, spareQueued)
}

// CompletionRead is the console as read once for several completion checks,
// such as one idle sweep's: each check reuses the read while the console
// has not been written since, and reads it again in its own transaction
// once it has. Every console write moves the store's revision, so an
// unchanged revision is an unchanged console. Checks must run in
// transactions that have not written the console themselves. A
// CompletionRead is not safe for concurrent use.
type CompletionRead struct {
	revision uint64
	state    DurableState
	err      error
	read     bool
}

// ReadCompletion reads the console of book outside any transaction. A read
// that fails is left for the first check to make in its transaction.
func ReadCompletion(book *ledger.Ledger) *CompletionRead {
	r := &CompletionRead{}
	if records, err := loadConsoleRecords(book); err == nil {
		r.keep(records)
	}
	return r
}

func (r *CompletionRead) keep(records consoleRecords) {
	r.revision, r.read = records.revision, true
	r.state, r.err = records.state()
}

// CheckTaskCompletionTx is the package's CheckTaskCompletionTx over the read,
// once the transaction's console revision shows the read is current.
func (r *CompletionRead) CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string, spareQueued bool) error {
	revision, err := consoleRevisionTx(tx)
	if err != nil {
		return fmt.Errorf("read completion console: %w", err)
	}
	if !r.read || revision != r.revision {
		records, err := loadConsoleRecordsTx(tx)
		if err != nil {
			return fmt.Errorf("read completion console: %w", err)
		}
		r.keep(records)
	}
	if r.err != nil {
		return fmt.Errorf("read completion console: %w", r.err)
	}
	return checkTaskCompletion(tx, r.state, ids, conversation, currentExchange, spareQueued)
}

// An exchange whose stop is requested and only waits to be confirmed, of a
// task the store has cancelled or paused, is left out of what the
// conversation holds, with the card it waits on: nothing the owner answers
// there settles these tasks, and the stop goes on being confirmed after
// they end.
func checkTaskCompletion(tx ledger.Reader, state DurableState, ids map[string]bool, conversation, currentExchange string, spareQueued bool) error {
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
	settling := map[string]bool{}
	for _, list := range state.Exchanges {
		for _, exchange := range list {
			if exchange.Conversation != conversation {
				continue
			}
			ok, err := stopSettling(tx, exchange.State, exchange.RecoveryPending, exchange.RecoveryStopPending, exchange.RecoveryStopTask)
			if err != nil {
				return fmt.Errorf("read completion tasks: %w", err)
			}
			if ok {
				settling[exchange.ID] = true
			}
		}
	}
	for _, question := range state.Questions {
		if question.State == "pending" && question.Conversation == conversation && !settling[question.ExchangeID] {
			return fmt.Errorf("%w: question %s: %w", task.ErrCompleteAttention, question.ID, task.ErrCompleteConversation)
		}
	}
	for _, list := range state.Exchanges {
		for _, exchange := range list {
			if exchange.Conversation != conversation || exchange.State.Terminal() || settling[exchange.ID] {
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
