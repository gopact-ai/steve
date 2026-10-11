package work

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/interaction"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
	"testing"
)

func TestWorkReplayKeepsOriginalTaskAttemptAndUnknownExecutionCannotRestart(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "settled"}[settle], func(t *testing.T) {
			engine, req, tracked, a, attempts := workExecutionFixture(t)
			book := engine.Book
			execute := func(ctx context.Context, r turnapi.Request) (turnapi.Result, error) {
				r.OnTurnReady(tracked.ID, a.ID)
				if settle {
					if _, err := attempts.Finish(ctx, a.ID, "hub", attempt.Result{Summary: "original result"}); err != nil {
						return turnapi.Result{}, err
					}
					return turnapi.Result{Attempt: a.ID, Text: "original result"}, nil
				}
				return turnapi.Result{}, channel.ErrOutcomeUnknown
			}
			if _, err := engine.Run(t.Context(), req, execute); settle && err != nil {
				t.Fatal(err)
			}
			replay, err := engine.Run(t.Context(), req, func(context.Context, turnapi.Request) (turnapi.Result, error) {
				t.Fatal("replay started a replacement execution")
				return turnapi.Result{}, nil
			})
			if settle {
				if err != nil || replay.Attempt != a.ID || replay.Text != "original result" {
					t.Fatalf("replay lost original output: %+v %v", replay, err)
				}
			} else if !errors.Is(err, channel.ErrOutcomeUnknown) {
				t.Fatalf("uncertain execution treated as safe replay: %v", err)
			}
			record, found, err := durable.Read[InputRecord](t.Context(), book, InputKind, durable.Key("third", "native-conversation", "native-message"))
			if err != nil || !found || record.Task != tracked.ID || record.Attempt != a.ID || record.Actor != (identity.Account{Channel: "third", ID: "actor"}) {
				t.Fatalf("input lost original native authority: %+v %v", record, err)
			}
			req.Input = "different request"
			if _, err := engine.Run(t.Context(), req, execute); !errors.Is(err, ledger.ErrConflict) {
				t.Fatalf("immutable input changed: %v", err)
			}
		})
	}
}

func TestRestartedInteractionCannotReturnAnApproval(t *testing.T) {
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
		if err := identity.BindExecutionTx(tx, "original", identity.Account{Channel: "third", ID: "actor"}); err != nil {
			return err
		}
		return tx.PutBinding(interaction.RuntimeKind, "active", "one")
	}); err != nil {
		t.Fatal(err)
	}
	service := &interaction.Service{Book: book, Runtime: "one"}
	_, err = service.Request(t.Context(), interaction.Origin{Work: "work", Address: channel.Address{Channel: "third", Conversation: "conversation", Message: "message"}, Actor: identity.Account{Channel: "third", ID: "actor"}, Task: tracked.ID, Attempt: "original"}, "permission", map[string]string{"tool": "write"}, func() (any, error) {
		if err := book.PutBinding(t.Context(), interaction.RuntimeKind, "active", "two"); err != nil {
			t.Fatal(err)
		}
		replacement := &interaction.Service{Book: book, Runtime: "two"}
		if err := replacement.Pump(t.Context()); err != nil {
			t.Fatal(err)
		}
		return "allow", nil
	})
	if err == nil {
		t.Fatal("late answer from old presentation granted approval")
	}
	records, err := service.Records(t.Context(), "")
	if err != nil || len(records.Items) != 1 || records.Items[0].State != "interrupted" || len(records.Items[0].Response) != 0 {
		t.Fatalf("lost interrupted question evidence: %+v %v", records, err)
	}
}

func workExecutionFixture(t *testing.T) (*Engine, turnapi.Request, task.Task, attempt.Record, *attempt.Service) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "native-conversation", Transport: "third", Requester: "actor", ProjectID: "p", Member: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = tasks.Begin(tracked.ID, "builder", "hub", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	a, err := attempts.Open(t.Context(), attempt.Spec{TaskID: tracked.ID, TurnID: "native-message", Kind: attempt.KindChat, Project: "p", Agent: "builder", By: "actor", Execution: &token, Scope: attempt.ScopeUnrestricted, Workspace: project.Workspace{ID: "canonical:p", Project: "p", Path: t.TempDir(), Kind: project.KindCanonical}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, a.ID, a.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := attempts.Advance(t.Context(), a.ID, state, "hub", nil); err != nil {
			t.Fatal(err)
		}
	}
	engine := &Engine{Book: book, Runtime: "one"}
	req := turnapi.Request{Source: turnapi.Source{Channel: "third", ConversationID: "native-conversation", MessageID: "native-message"}, Actor: turnapi.Actor{ID: "actor"}, Input: "original request"}

	return engine, req, tracked, a, attempts
}

func TestWorkCannotInheritAnAccountBindingChangedAfterAcceptance(t *testing.T) {
	engine, req, tracked, a, _ := workExecutionFixture(t)
	_, err := engine.Run(t.Context(), req, func(ctx context.Context, r turnapi.Request) (turnapi.Result, error) {
		accounts := &identity.Accounts{Book: engine.Book}
		account := identity.Account{Channel: "third", ID: "actor"}
		if _, err := accounts.Bind(ctx, account, (identity.Account{Channel: "console", ID: "owner"}).Principal(), 0, true); err != nil {
			t.Fatal(err)
		}
		r.OnTurnReady(tracked.ID, a.ID)
		if ctx.Err() == nil {
			t.Fatal("changed account authority did not cancel dispatch")
		}
		return turnapi.Result{}, ctx.Err()
	})
	if !errors.Is(err, identity.ErrDenied) || !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatalf("input gained later account authority: %v", err)
	}
	proof, found, err := durable.Read[identity.ExecutionProof](t.Context(), engine.Book, identity.ExecutionKind, a.ID)
	if err != nil || found {
		t.Fatalf("changed authority was installed in original execution: %+v %v", proof, err)
	}
	if err := engine.Book.Read(t.Context(), func(tx *ledger.ReadTx) error { return CheckOutputAuthorityTx(tx, req.Address()) }); !errors.Is(err, identity.ErrDenied) {
		t.Fatalf("changed account received old privileged output: %v", err)
	}
}
