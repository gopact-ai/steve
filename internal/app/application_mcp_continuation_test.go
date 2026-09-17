package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// A conversation has to keep working after its previous turn ended, however it
// ended. The grant moves to the next independently authorized attempt as soon
// as the previous one can no longer act, and stays put while it might.
func TestApplicationMCPNextTurnFollowsAnyEndedAndSettledPreviousTurn(t *testing.T) {
	for _, mode := range []string{"completed", "resumed-context", "fresh-context", "cancelled-task", "paused-task", "failed-before-binding", "other-node", "still-running", "never-settled", "unsettled-writer", "superseded-elsewhere", "stopped-next-task"} {
		t.Run(mode, func(t *testing.T) {
			book, tasks := applicationGrantBook(t)
			gate := applicationGrantGate(t, book)
			gate.Extras("chat", "agent", "native-token", "")
			binding := agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}
			old, scope := openGrantAttempt(t, book, tasks, "original", "ns_original")
			service := attempt.New(book)
			if _, err := service.Advance(t.Context(), old.ID, attempt.Running, "test", nil); err != nil {
				t.Fatal(err)
			}
			if err := gate.BindExecution(t.Context(), binding, scope); err != nil {
				t.Fatal(err)
			}
			settle := mode != "still-running" && mode != "never-settled"
			if settle {
				if err := service.MarkSessionSettled(t.Context(), old.ID, "native settled"); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "failed-before-binding", "cancelled-task", "never-settled":
				if _, err := service.Fail(t.Context(), old.ID, "test", "agent canceled the turn"); err != nil {
					t.Fatal(err)
				}
			case "unsettled-writer":
				if _, err := service.Advance(t.Context(), old.ID, attempt.Failed, "test", func(r *attempt.Record) { r.Unsettled = true }); err != nil {
					t.Fatal(err)
				}
			case "superseded-elsewhere":
				if _, err := service.Advance(t.Context(), old.ID, attempt.Failed, "test", func(r *attempt.Record) { r.SupersededBy = "someone-else" }); err != nil {
					t.Fatal(err)
				}
			case "still-running":
			default:
				if _, err := service.Advance(t.Context(), old.ID, attempt.BindReady, "test", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Complete(t.Context(), old.ID, "test", attempt.Completion{Result: attempt.Result{Summary: "done"}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tasks.Finish(scope.TaskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "paused-task":
				if _, err := tasks.SetAside(scope.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
			case "cancelled-task":
				if _, err := tasks.SetAside(scope.TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			case "still-running", "never-settled", "unsettled-writer", "superseded-elsewhere":
			default:
				if _, err := tasks.CompleteRoot(t.Context(), scope.TaskID, "chat", func(tx *ledger.Tx, ids map[string]bool) error { return attempt.CheckTaskCompletionTx(tx, ids) }); err != nil {
					t.Fatal(err)
				}
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); mode != "still-running" && status != http.StatusUnauthorized {
				t.Fatalf("ended execution kept tools: %d", status)
			}
			node, session, context := "node-a", "ns_resumed", "ns_original"
			switch mode {
			case "resumed-context":
				session = "ns_original"
			case "fresh-context":
				context = ""
			case "other-node":
				node = "node-b"
			}
			next, nextScope := openGrantAttemptOn(t, book, tasks, "next", session, node)
			if _, err := service.Advance(t.Context(), next.ID, attempt.Running, "test", func(r *attempt.Record) { r.NativeContext = context }); err != nil {
				t.Fatal(err)
			}
			if mode == "stopped-next-task" {
				if _, err := tasks.SetAside(nextScope.TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			}
			err := gate.BindExecution(t.Context(), binding, nextScope)
			switch mode {
			case "still-running", "never-settled", "unsettled-writer", "superseded-elsewhere", "stopped-next-task":
				if !errors.Is(err, agentmcp.ErrGrantDenied) {
					t.Fatalf("unsafe transfer accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("next turn refused after the previous one ended: %v", err)
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusOK {
				t.Fatalf("new admitted execution has no tools: %d", status)
			}
			if err := gate.BindExecution(t.Context(), binding, scope); !errors.Is(err, agentmcp.ErrGrantDenied) {
				t.Fatalf("old scope revived: %v", err)
			}
			if err := tasks.CheckExecution(*old.Execution); mode != "paused-task" && !errors.Is(err, task.ErrExecutionStopped) {
				t.Fatalf("old execution fence weakened: %v", err)
			}
		})
	}
}
