package turn

import (
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ConsoleCompletionGuard checks console-owned attention and delivery facts
// within the transaction closing a task tree.
type ConsoleCompletionGuard func(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error

// SetConsoleCompletionGuard is wired during application assembly: console
// depends on turn, so turn cannot import the console's storage interpreter.
func (c *Coordinator) SetConsoleCompletionGuard(guard ConsoleCompletionGuard) {
	c.consoleCompletionGuard = guard
}

func (c *Coordinator) checkConsoleCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error {
	if c.consoleCompletionGuard != nil {
		return c.consoleCompletionGuard(tx, ids, conversation, currentExchange)
	}
	// A standalone turn coordinator needs no console. Existing console
	// facts, however, must never be silently ignored by an unwired owner.
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM bindings WHERE kind IN ('console-store','console-head','console-reply','console-exchange','console-question'))`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("task completion requires the console completion guard")
	}
	return nil
}
