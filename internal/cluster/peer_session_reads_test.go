package cluster

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func sessionReadFixture(t *testing.T) (*ledger.Ledger, *task.Store, task.Task, attempt.Record, nodewire.SessionBinding) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := tasks.Create(task.Task{Channel: "console:read-session", Member: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := tasks.Spawn(root.ID, task.Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := attempt.New(book).Open(t.Context(), attempt.Spec{
		ID: "read-session", TaskID: child.ID, Agent: child.Member, Node: "worker", Harness: "mock",
		Execution: &token, Scope: attempt.ScopePathSet,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := nodewire.SessionBinding{
		TaskID: child.ID, AttemptID: r.ID, NodeID: r.Node, TaskEpoch: token.Epoch,
		ExecutionEpoch: attempt.SessionExecutionEpoch(r), SessionID: LogicalAgentSession(child.Channel, child.ID, r.Agent),
	}
	return book, tasks, root, r, b
}

func TestSessionAuthorizationReadsOnlyBoundHeadersAndAncestors(t *testing.T) {
	book, tasks, root, r, binding := sessionReadFixture(t)
	// Deliberately unreadable history proves this path does not load or decode
	// the full owner store, even when deciding execution authority.
	if _, err := book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('task-attempt','unrelated','invalid','2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := task.OpenLedger(book, ""); err == nil {
		t.Fatal("fixture did not distinguish full loads from header reads")
	}
	got, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionStart)
	if err != nil || got.ID != r.ID {
		t.Fatalf("admitted execution refused: %+v %v", got, err)
	}
	wrong := binding
	wrong.SessionID = "another-conversation"
	if _, err := readSessionExecution(t.Context(), book, wrong, nodewire.SessionActionStart); err == nil {
		t.Fatal("accepted a different conversation")
	}
	wrong = binding
	wrong.TaskEpoch++
	if _, err := readSessionExecution(t.Context(), book, wrong, nodewire.SessionActionStart); err == nil {
		t.Fatal("accepted an uncommitted epoch")
	}
	if _, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionPrompt); err == nil {
		t.Fatal("leased execution could prompt before running")
	}
	if _, err := tasks.Advance(root.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionStart); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("ancestor stop did not revoke child admission: %v", err)
	}
	for _, action := range []nodewire.SessionAction{nodewire.SessionActionAttach, nodewire.SessionActionCancel, nodewire.SessionActionClose} {
		if _, err := readSessionExecution(t.Context(), book, binding, action); err != nil {
			t.Fatalf("stop/observation lost its original binding (%s): %v", action, err)
		}
	}
}

func TestSessionAuthorizationUsesCurrentLeasesAndExecutionPhase(t *testing.T) {
	book, _, _, r, binding := sessionReadFixture(t)
	attempts := attempt.New(book)
	if _, err := attempts.Advance(t.Context(), r.ID, attempt.Prepared, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Advance(t.Context(), r.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionPrompt); err != nil {
		t.Fatal(err)
	}
	if err := book.Release(t.Context(), r.Leases[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionPrompt); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("released lease could authorize native input: %v", err)
	}
	if _, err := readSessionExecution(t.Context(), book, binding, nodewire.SessionActionAbort); err != nil {
		t.Fatalf("revoked lease prevented cleanup: %v", err)
	}
	if _, err := readSessionExecution(t.Context(), book, binding, "unknown"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestSessionAuthorizationRechecksExecutionAfterRuntimeScopeRead(t *testing.T) {
	book, tasks, root, _, binding := sessionReadFixture(t)
	err := authorizeSessionRead(t.Context(), book, binding, nodewire.SessionActionStart, func(attempt.Record) error {
		// The plugin owner uses its own ledger reads. It must be outside the
		// read transaction, and task/lease authorization must follow it.
		_, err := tasks.Advance(root.ID, task.StatePaused)
		return err
	})
	if !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("scope preflight raced a stop but still admitted execution: %v", err)
	}
	err = authorizeSessionRead(t.Context(), book, binding, nodewire.SessionActionClose, func(attempt.Record) error {
		t.Fatal("cleanup must not require current plugin scope")
		return nil
	})
	if err != nil {
		t.Fatalf("scope changes prevented cleanup: %v", err)
	}
}

func TestSessionAuthorizationCannotReuseAnotherRuntimeScopeCheck(t *testing.T) {
	book, _, _, r, binding := sessionReadFixture(t)
	err := authorizeSessionRead(t.Context(), book, binding, nodewire.SessionActionStart, func(attempt.Record) error {
		// An unchanged (empty) runtime ID must not hide a different body.
		// The final read must authorize exactly the checked runtime reference.
		_, err := book.DB().Exec(`UPDATE operations SET data=json_set(data,'$.plugin_runtime',json('{"id":"","selection":{}}')) WHERE id=?`, r.ID)
		return err
	})
	if err == nil {
		t.Fatal("execution used a runtime reference its scope check never saw")
	}
}
