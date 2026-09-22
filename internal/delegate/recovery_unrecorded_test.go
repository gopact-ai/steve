package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// unrecordedChild leaves a delegated child the way a hub that died between
// run's Begin and the attempt's admission leaves it: an open accounting row
// under the parent, and no attempt record in the ledger that will ever
// settle it. With bound, the row already names the attempt that was never
// written.
func unrecordedChild(t *testing.T, w *world, parent task.Task, bound bool) task.Task {
	t.Helper()
	child, err := w.tasks.Spawn(parent.ID, task.Task{Goal: "work", Member: "builder", Node: "node-a", Origin: "delegate:" + parent.ID, ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Begin(child.ID, "builder", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	if bound {
		token, err := w.tasks.ExecutionToken(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.tasks.BindAttempt(token, "att-never-written", "delegate/"+child.ID); err != nil {
			t.Fatal(err)
		}
	}
	return child
}

func TestRecoverRetainedSettlesAChildThatNeverReachedTheLedger(t *testing.T) {
	for name, bound := range map[string]bool{"unbound row": false, "row bound to a missing attempt": true} {
		t.Run(name, func(t *testing.T) {
			w, _ := executionWorld(t)
			parent := w.running(t, "codex")
			child := unrecordedChild(t, w, parent, bound)
			// The parent's own turn is over; only the child's row keeps the
			// root from being completed.
			if _, err := w.tasks.Finish(parent.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
				t.Fatal(err)
			}
			root, _ := w.tasks.Get(parent.ID)
			if err := task.CompletionBlocker(root, w.tasks.List("")); !errors.Is(err, task.ErrCompleteBusy) {
				t.Fatalf("completion blocker before recovery = %v, want %v", err, task.ErrCompleteBusy)
			}
			time.Sleep(5 * time.Millisecond)

			// The next process finds the row open with nothing behind it.
			service := recoveredDelegateService(t, w, w.sessions)
			box := &mailbox{}
			service.SetDeliverer(box.deliver)
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}

			stored, _ := w.tasks.Get(child.ID)
			if len(stored.Attempts) != 1 || stored.Attempts[0].Open() || stored.Attempts[0].Outcome != task.OutcomeInterrupted {
				t.Fatalf("accounting rows = %+v", stored.Attempts)
			}
			if stored.State != task.StateFailed || stored.Result == nil || stored.Result.Outcome != task.OutcomeInterrupted || stored.Result.Answer == "" {
				t.Fatalf("child = %+v result = %+v", stored, stored.Result)
			}
			if bound && stored.Result.Attempt != "att-never-written" {
				t.Fatalf("result names attempt %q, want the bound one", stored.Result.Attempt)
			}
			// Nothing ran, so no time is charged for the outage: the child
			// spent none, and the parent only what its own turn took.
			charged, _ := w.tasks.Get(parent.ID)
			if stored.Budget.Elapsed != 0 || charged.Budget.Elapsed != root.Budget.Elapsed {
				t.Fatalf("elapsed charged: child %s parent %s (own turn %s)", stored.Budget.Elapsed, charged.Budget.Elapsed, root.Budget.Elapsed)
			}
			service.RedeliverPending(t.Context())
			got := box.wait(t, 1)
			if got[0].ParentTask != parent.ID || len(got[0].Children) != 1 || got[0].Children[0].Task != child.ID || got[0].Children[0].State != task.StateFailed {
				t.Fatalf("delivery = %+v", got[0])
			}
			// What blocks completion now is a failed child the user can look
			// at and settle by hand, the same as any child whose run failed —
			// not an execution nobody is running.
			root, _ = w.tasks.Get(parent.ID)
			if err := task.CompletionBlocker(root, w.tasks.List("")); !errors.Is(err, task.ErrCompleteChildren) {
				t.Fatalf("completion blocker after recovery = %v, want %v", err, task.ErrCompleteChildren)
			}
			if _, err := w.tasks.Settle(child.ID, task.SettlementHandled); err != nil {
				t.Fatal(err)
			}
			if err := task.CompletionBlocker(root, w.tasks.List("")); err != nil {
				t.Fatalf("completion blocker after settling the child = %v", err)
			}
			// Settled once: a later pass finds nothing to do and tells nobody twice.
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			service.RedeliverPending(t.Context())
			if box.count() != 1 {
				t.Fatalf("delivered %d times", box.count())
			}
		})
	}
}

// A row opened after the service started is some run's own, in the window
// before its attempt is admitted — a delegation of this process, or a chat
// turn the user resumed the child into — and is not recovery's to close.
func TestRecoverRetainedLeavesRowsOpenedSinceItStarted(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	spawned := unrecordedChild(t, w, parent, false)

	if err := w.service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(spawned.ID)
	if stored.State != task.StateRunning || !stored.Attempts[0].Open() || stored.Result != nil {
		t.Fatalf("a row being opened was settled: %+v result=%+v", stored, stored.Result)
	}
}

// An inherited row whose attempt is in the ledger belongs to that
// attempt's recovery, whatever state the attempt is in.
func TestRecoverRetainedLeavesAnInheritedRowWithItsRecord(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	spawned := unrecordedChild(t, w, parent, false)
	record, err := w.attempts.Open(t.Context(), attempt.Spec{TaskID: spawned.ID, TurnID: "delegate/" + spawned.ID, Kind: attempt.KindDelegate, Project: "p", Agent: "builder", Harness: "mock",
		Workspace: project.Workspace{ID: "w", Project: "p", Path: w.home, Kind: project.KindWorktree}, Scope: attempt.ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	token, err := w.tasks.ExecutionToken(spawned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.BindAttempt(token, record.ID, "delegate/"+spawned.ID); err != nil {
		t.Fatal(err)
	}

	service := recoveredDelegateService(t, w, w.sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(spawned.ID)
	if stored.State != task.StateRunning || !stored.Attempts[0].Open() || stored.Result != nil {
		t.Fatalf("a row with its record was settled: %+v result=%+v", stored, stored.Result)
	}
}

// A child whose run already failed and answered, but left the row it
// opened, is owed only the row: its result stands.
func TestRecoverRetainedClosesOnlyTheRowOfAChildThatAlreadyFailed(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	spawned := unrecordedChild(t, w, parent, false)
	if _, err := w.tasks.Advance(spawned.ID, task.StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(spawned.ID, task.Result{Outcome: task.OutcomeError, Answer: "bind delegate accounting: no"}); err != nil {
		t.Fatal(err)
	}

	service := recoveredDelegateService(t, w, w.sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(spawned.ID)
	if stored.Attempts[0].Open() || stored.Attempts[0].Outcome != task.OutcomeInterrupted {
		t.Fatalf("row left open: %+v", stored.Attempts)
	}
	if stored.State != task.StateFailed || stored.Result == nil || stored.Result.Answer != "bind delegate accounting: no" {
		t.Fatalf("the run's own result was overwritten: %+v", stored.Result)
	}
}

// A child its owner paused since the row was opened is left paused, with
// the row closed and nothing said for it: the next resume opens a row of
// its own.
func TestRecoverRetainedLeavesAPausedChildToItsOwner(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	spawned := unrecordedChild(t, w, parent, false)
	if _, err := w.tasks.SetAside(spawned.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}

	service := recoveredDelegateService(t, w, w.sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(spawned.ID)
	if stored.Attempts[0].Open() || stored.State != task.StatePaused || stored.Result != nil {
		t.Fatalf("paused child = %+v result=%+v", stored, stored.Result)
	}
	if _, err := w.tasks.Resume(spawned.ID, stored.ExecutionEpoch, task.StatePaused, task.ResumeAdmission{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Begin(spawned.ID, "builder", "node-a", ""); err != nil {
		t.Fatalf("the resumed child cannot open its own row: %v", err)
	}
}

// A child its owner resumed while the old row was still open opens its
// resumed turn past it: the stop revoked the old row's epoch, so Begin
// closes it as a turn that never ran. When the hub dies before that turn's
// attempt is admitted, its own row is the one left behind; the child ends
// as interrupted so its parent is told, and a turn still queued for it
// opens from there.
func TestRecoverRetainedFailsAResumedChildWhoseTurnNeverRan(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	spawned := unrecordedChild(t, w, parent, false)
	if _, err := w.tasks.SetAside(spawned.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	paused, _ := w.tasks.Get(spawned.ID)
	if _, err := w.tasks.Resume(spawned.ID, paused.ExecutionEpoch, task.StatePaused, task.ResumeAdmission{}); err != nil {
		t.Fatal(err)
	}
	resumed, err := w.tasks.Begin(spawned.ID, "builder", "node-a", "")
	if err != nil {
		t.Fatalf("the revoked row stood in the way of the resumed turn: %v", err)
	}
	if len(resumed.Attempts) != 2 || resumed.Attempts[0].Open() || !resumed.Attempts[0].EndedAt.Equal(resumed.Attempts[0].StartedAt) {
		t.Fatalf("revoked row = %+v", resumed.Attempts)
	}

	service := recoveredDelegateService(t, w, w.sessions)
	box := &mailbox{}
	service.SetDeliverer(box.deliver)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(spawned.ID)
	if stored.Attempts[0].Open() || stored.State != task.StateFailed || stored.Result == nil || stored.Result.Outcome != task.OutcomeInterrupted {
		t.Fatalf("resumed child = %+v result=%+v", stored, stored.Result)
	}
	service.RedeliverPending(t.Context())
	if got := box.wait(t, 1); got[0].Children[0].Task != spawned.ID || got[0].Children[0].State != task.StateFailed {
		t.Fatalf("delivery = %+v", got[0])
	}
	if _, err := w.tasks.Begin(spawned.ID, "builder", "node-a", ""); err != nil {
		t.Fatalf("a turn still queued for the child cannot open its row: %v", err)
	}
}

// staleEndpoints moves the child's execution epoch on while its run is
// between spawning and opening the accounting row, so the run's bind — made
// under the epoch it was admitted with — is refused.
type staleEndpoints struct {
	tasks  *task.Store
	parent string
}

func (e staleEndpoints) MCPEndpoint(context.Context, string) (string, error) {
	for _, child := range e.tasks.Children(e.parent) {
		if _, err := e.tasks.SetAside(child.ID, task.StatePaused); err != nil {
			return "", err
		}
		if _, err := e.tasks.Advance(child.ID, task.StateRunning); err != nil {
			return "", err
		}
	}
	return "http://127.0.0.1:4545/mcp", nil
}

// A run whose bind is refused closes the row its Begin opened: nothing will
// ever admit an attempt against it, and open it would block the tree.
func TestDelegateClosesItsRowWhenTheBindIsRefused(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	w.service.SetEndpoints(staleEndpoints{tasks: w.tasks, parent: parent.ID})

	_, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Requires: []string{"gpu"}})
	if err == nil || !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("delegate err = %v, want the refused bind", err)
	}
	children := w.tasks.Children(parent.ID)
	if len(children) != 1 {
		t.Fatalf("children = %+v", children)
	}
	stored := awaitDelegateResult(t, w.tasks, children[0].ID)
	if len(stored.Attempts) != 1 || stored.Attempts[0].Open() || stored.Attempts[0].Outcome != task.OutcomeError {
		t.Fatalf("accounting rows = %+v", stored.Attempts)
	}
	if stored.Result.Outcome != task.OutcomeError || stored.Result.Answer == "" {
		t.Fatalf("result = %+v", stored.Result)
	}
	if err := task.CompletionBlocker(parent, w.tasks.List("")); errors.Is(err, task.ErrCompleteBusy) {
		t.Fatalf("completion blocker = %v", err)
	}
}
