package turn

import (
	"errors"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
)

// ConsoleCompletionGuard checks console-owned attention and delivery facts
// within the transaction closing a task tree. Console depends on turn, so
// turn cannot import the console's storage interpreter; the application
// passes it in Deps.
type ConsoleCompletionGuard func(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error

// CheckTaskCompletionTx refuses, within tx, to let the tasks in ids end
// while any of them still has something unsettled: an execution running,
// reserved or not confirmed idle, a result or landing not yet delivered, an
// answer or reconciliation still owed. /complete checks it, and so does
// every path that ends a task as done without anyone claiming the work
// succeeded — a session reset, a project switch, an idle close, a
// schedule's next run — so none of them can end a task its continuation
// still runs under. console checks what the conversation's console holds,
// sparing its currentExchange, the input that asked for the close.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string, console ConsoleCompletionGuard) error {
	if err := attempt.CheckTaskCompletionTx(tx, ids); err != nil {
		return err
	}
	if err := artifact.CheckTaskLandingsTx(tx, ids); err != nil {
		return err
	}
	if err := intent.CheckTaskCompletionTx(tx, ids); err != nil {
		return err
	}
	if err := project.CheckTaskCompletionTx(tx, ids); err != nil {
		return err
	}
	if err := plan.CheckTaskCompletionTx(tx, ids); err != nil {
		return err
	}
	return console(tx, ids, conversation, currentExchange)
}

func (c *Coordinator) checkTaskCompletionTx(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error {
	return CheckTaskCompletionTx(tx, ids, conversation, currentExchange, c.checkConsoleCompletionTx)
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
