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

func TestApplicationMCPAcceptedChatContinuesOnlyWithVerifiedNativeContext(t *testing.T) {
	for _, mode := range []string{"warm", "cold", "missing-proof", "wrong-context", "warm-missing-proof", "warm-wrong-context", "pause", "cancel", "older-epoch"} {
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
			if err := service.MarkSessionSettled(t.Context(), old.ID, "native completed"); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Advance(t.Context(), old.ID, attempt.BindReady, "test", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Complete(t.Context(), old.ID, "test", attempt.Completion{Result: attempt.Result{Summary: "done"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Finish(scope.TaskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "pause":
				if _, err := tasks.SetAside(scope.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if _, err := tasks.SetAside(scope.TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			default:
				if mode == "older-epoch" {
					if _, err := tasks.SetAside(scope.TaskID, task.StatePaused); err != nil {
						t.Fatal(err)
					}
					if _, err := tasks.Advance(scope.TaskID, task.StateRunning); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tasks.CompleteRoot(t.Context(), scope.TaskID, "chat", func(tx *ledger.Tx, ids map[string]bool) error { return attempt.CheckTaskCompletionTx(tx, ids) }); err != nil {
					t.Fatal(err)
				}
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusUnauthorized {
				t.Fatalf("closed old execution kept tools: %d", status)
			}
			session, context := "ns_resumed", "ns_original"
			if mode == "warm" || mode == "warm-missing-proof" || mode == "warm-wrong-context" {
				session = "ns_original"
			}
			if mode == "missing-proof" || mode == "warm-missing-proof" {
				context = ""
			}
			if mode == "wrong-context" || mode == "warm-wrong-context" {
				context = "ns_unrelated"
			}
			next, nextScope := openGrantAttempt(t, book, tasks, "next", session)
			if _, err := service.Advance(t.Context(), next.ID, attempt.Running, "test", func(r *attempt.Record) { r.NativeContext = context }); err != nil {
				t.Fatal(err)
			}
			err := gate.BindExecution(t.Context(), binding, nextScope)
			if mode != "warm" && mode != "cold" {
				if !errors.Is(err, agentmcp.ErrGrantDenied) {
					t.Fatalf("unsafe transfer accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("accepted native context refused: %v", err)
			}
			if status := grantRPC(t, gate, "native-token", "tools/call"); status != http.StatusOK {
				t.Fatalf("new admitted execution has no tools: %d", status)
			}
			if err := gate.BindExecution(t.Context(), binding, scope); !errors.Is(err, agentmcp.ErrGrantDenied) {
				t.Fatalf("old scope revived: %v", err)
			}
			if err := tasks.CheckExecution(*old.Execution); !errors.Is(err, task.ErrExecutionStopped) {
				t.Fatalf("old execution fence weakened: %v", err)
			}
		})
	}
}
