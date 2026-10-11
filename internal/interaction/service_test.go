package interaction

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestEndedOriginalTaskRetiresAnUndeadlinedQuestionBeforeLateAnswer(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "conversation", Transport: "third", Requester: "actor", ProjectID: "p", Member: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	a, err := attempts.Open(t.Context(), attempt.Spec{ID: "original", TaskID: tracked.ID, TurnID: "message", Kind: attempt.KindChat, Project: "p", By: "actor", Execution: &token, Scope: attempt.ScopeUnrestricted, Workspace: project.Workspace{ID: "canonical:p", Project: "p", Kind: project.KindCanonical, Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := attempts.Advance(t.Context(), a.ID, state, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := identity.BindExecutionTx(tx, a.ID, identity.Account{Channel: "third", ID: "actor"}); err != nil {
			return err
		}
		return tx.PutBinding(RuntimeKind, "active", "one")
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Book: book, Runtime: "one"}
	_, err = service.Request(context.WithoutCancel(t.Context()), Origin{Work: "work", Address: channel.Address{Channel: "third", Conversation: "conversation", Message: "message"}, Actor: identity.Account{Channel: "third", ID: "actor"}, Task: tracked.ID, Attempt: a.ID}, "permission", map[string]string{"tool": "write"}, func() (any, error) {
		if _, err := tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
			t.Fatal(err)
		}
		if err := service.Pump(t.Context()); err != nil {
			t.Fatal(err)
		}
		return "allow", nil
	})
	if err == nil {
		t.Fatal("ended original execution accepted a late permission")
	}
	records, err := service.Records(t.Context(), "")
	if err != nil || len(records.Items) != 1 || records.Items[0].State != "interrupted" || len(records.Items[0].Response) != 0 {
		t.Fatalf("stale question remained active: %+v %v", records, err)
	}
}
