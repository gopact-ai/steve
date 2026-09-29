package turn

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

// A refused reset or project switch says why, in the terms of the check
// that refused it, and how to let the task go.
func TestCloseRefusalSaysWhy(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	for _, tc := range []struct {
		name string
		err  error
		key  i18n.Key
	}{
		{"busy", fmt.Errorf("attempt att-1: %w", task.ErrCompleteBusy), i18n.TaskCloseBusy},
		{"delivery", fmt.Errorf("exchange e1: %w", task.ErrCompleteDelivery), i18n.TaskCloseDelivery},
		{"attention", fmt.Errorf("question q1: %w", task.ErrCompleteAttention), i18n.TaskCloseAttention},
		{"unverified", errors.New("ledger unavailable"), i18n.TaskCloseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := c.closeRefusal("chat", "7", tc.err)
			if want := c.text.T(tc.key, "7", protocol.CommandTasks); got.Text != want {
				t.Fatalf("refusal for %v = %q, want %q", tc.err, got.Text, want)
			}
		})
	}
}

// A refused close names the task it is about, the one the user is told
// to cancel. When the check refuses a task, it is that one. When closing
// several fails otherwise — one of them can no longer end, the change is
// not saved — none of them is to blame more than the others, and the
// refusal names none.
func TestCloseSettledNamesOnlyTheTaskItIsAbout(t *testing.T) {
	var refuse string
	c, tasks, book := taskCoordinatorBook(t, &fakeRunner{reply: "ok"}, withDeps(func(d *Deps) {
		d.ConsoleCompletionGuard = func(_ *ledger.Tx, ids map[string]bool, _, _ string, _ bool) error {
			if ids[refuse] {
				return task.ErrCompleteAttention
			}
			return nil
		}
	}))
	open := func() string {
		t.Helper()
		created, err := tasks.Create(task.Task{Goal: "chat", Channel: "chat", Member: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	first, second, cancelled := open(), open(), open()
	if _, err := tasks.Advance(cancelled, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	closing := func(name string, ids []string, want string) {
		t.Helper()
		refused, err := c.closeSettled(t.Context(), ids, "")
		if err == nil || refused != want {
			t.Errorf("%s: close of %v refused %q with %v, want %q named", name, ids, refused, err, want)
		}
	}

	refuse = second
	closing("the check refuses the second", []string{first, second}, second)
	refuse = ""
	closing("the second can no longer end", []string{first, cancelled}, "")

	if _, err := book.DB().Exec(`CREATE TRIGGER reject_close BEFORE UPDATE ON bindings WHEN NEW.kind = 'task-store' AND NEW.id = 'state' BEGIN SELECT RAISE(ABORT, 'close write unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	closing("the close is not saved", []string{first, second}, "")
	closing("the close of one alone is not saved", []string{first}, first)
	for _, id := range []string{first, second} {
		if tracked, _ := tasks.Get(id); tracked.State != task.StateDraft {
			t.Fatalf("task %s after refused closes = %s, want it as it was", id, tracked.State)
		}
	}
}
