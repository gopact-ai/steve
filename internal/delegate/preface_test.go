package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// A user's stop is the last word: a child that ends while the task is
// held is not delivered — delivery would start the task's next turn on
// its own — and waits for the user's next turn instead.
func TestFlushDeliversNothingWhileTheParentIsHeld(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	held, err := w.tasks.Hold(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	waitForResult(t, w, first.TaskID)
	time.Sleep(100 * time.Millisecond)
	w.service.Flush(t.Context(), parent.ID)
	w.service.RedeliverPending(t.Context())
	if box.count() != 0 {
		t.Fatalf("a held parent was woken %d time(s)", box.count())
	}
	if c, _ := w.tasks.Get(first.TaskID); c.Delivery != nil && c.Delivery.State != task.DeliveryPending {
		t.Fatalf("child delivery = %+v; want still owed", c.Delivery)
	}
	// The user speaks again: from then on the child's result travels as
	// it always did.
	if _, err := w.tasks.ReleaseHold(parent.ID, held.HeldAt); err != nil {
		t.Fatal(err)
	}
	w.service.Flush(t.Context(), parent.ID)
	if got := box.wait(t, 1); got[0].Children[0].Task != first.TaskID {
		t.Fatalf("delivery = %+v", got[0])
	}
}

// The task's next turn opens with what its children left: the ones the
// stop cancelled, with their partial answers; the ones that ended while
// it was held; and the ones a pause left paused, which are closed for
// good here — nothing resumes a delegation. Each is told once.
func TestPrefaceReportsStoppedChildrenOnceAndClosesPausedOnes(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	spawn := func(goal string, state task.State, result *task.Result) task.Task {
		t.Helper()
		child, err := w.tasks.Spawn(parent.ID, task.Task{Goal: goal, Member: "builder", Node: "node-a", Origin: "delegate:" + parent.ID})
		if err != nil {
			t.Fatal(err)
		}
		if state == task.StatePaused || state == task.StateCancelled {
			if _, err := w.tasks.SetAside(child.ID, state); err != nil {
				t.Fatal(err)
			}
		} else if _, err := w.tasks.Advance(child.ID, state); err != nil {
			t.Fatal(err)
		}
		if result != nil {
			if err := w.tasks.SetResult(child.ID, *result); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := w.tasks.Get(child.ID)
		return got
	}
	stopped := spawn("draft the schema", task.StateCancelled, &task.Result{Outcome: task.OutcomeCancelled, Answer: "got halfway through the tables", Refs: []string{"git abc123"}})
	finished := spawn("write the tests", task.StateDone, &task.Result{Outcome: task.OutcomeOK, Answer: "tests are green"})
	paused := spawn("review the API", task.StatePaused, &task.Result{Outcome: task.OutcomeCancelled, Answer: "read half of it"})
	unconfirmed := spawn("migrate the data", task.StateCancelled, nil)
	held, err := w.tasks.Hold(parent.ID)
	if err != nil {
		t.Fatal(err)
	}

	text, told := w.service.Preface(t.Context(), parent.ID)
	for _, want := range []string{
		"#" + stopped.ID, "已取消", "got halfway through the tables", "git abc123",
		"#" + finished.ID, "完成", "tests are green",
		"#" + paused.ID, "read half of it",
		"#" + unconfirmed.ID, "没有确认",
		"不是用户说的", "steve_delegate",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("preface lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "已暂停") || strings.Contains(text, "paused") {
		t.Fatalf("a paused child was presented as resumable:\n%s", text)
	}
	// Composed is not yet told: until the agent has the prompt, the
	// children still owe their parent, the paused one is still paused,
	// and a repeat says the same things.
	for _, id := range []string{stopped.ID, finished.ID, paused.ID} {
		if c, _ := w.tasks.Get(id); c.Delivery != nil && c.Delivery.State == task.DeliveryDelivered {
			t.Fatalf("child #%s was marked delivered before the agent had the prompt", id)
		}
	}
	if c, _ := w.tasks.Get(paused.ID); c.State != task.StatePaused {
		t.Fatalf("paused child = %s before the agent had the prompt; want still paused", c.State)
	}
	if repeat, _ := w.service.Preface(t.Context(), parent.ID); !strings.Contains(repeat, "#"+stopped.ID) || !strings.Contains(repeat, "#"+paused.ID) {
		t.Fatalf("an untold preface was not repeated:\n%s", repeat)
	}
	told()
	for _, id := range []string{stopped.ID, finished.ID, paused.ID} {
		c, _ := w.tasks.Get(id)
		if c.Delivery == nil || c.Delivery.State != task.DeliveryDelivered {
			t.Fatalf("child #%s delivery = %+v; want delivered once told", id, c.Delivery)
		}
	}
	if c, _ := w.tasks.Get(paused.ID); c.State != task.StateCancelled {
		t.Fatalf("paused child = %s once told; want closed as cancelled", c.State)
	}
	if c, _ := w.tasks.Get(unconfirmed.ID); c.Result != nil || c.Delivery != nil {
		t.Fatalf("an unconfirmed stop was given a result or a delivery: %+v", c)
	}
	// Told once: the hold lifts, and neither a later preface nor a flush
	// repeats any of it.
	if _, err := w.tasks.ReleaseHold(parent.ID, held.HeldAt); err != nil {
		t.Fatal(err)
	}
	if again, _ := w.service.Preface(t.Context(), parent.ID); again != "" {
		t.Fatalf("a told preface repeated:\n%s", again)
	}
	w.service.Flush(t.Context(), parent.ID)
	w.service.RedeliverPending(t.Context())
	time.Sleep(100 * time.Millisecond)
	if box.count() != 0 {
		t.Fatalf("flush re-sent %d delivery(ies) after the preface", box.count())
	}
}

// A child that finished while the task was held left its files queued.
// The next turn lands them under its own lease — the parent works with
// them in this turn — and the preface says so.
func TestPrefaceLandsQueuedFilesUnderTheTurnsLease(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	child, ok := w.tasks.Get(first.TaskID)
	if !ok || child.Workspace == "" {
		t.Fatalf("child workspace unknown: %+v", child)
	}
	if err := os.WriteFile(filepath.Join(child.Workspace, "notes.md"), []byte("by the child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Hold(parent.ID); err != nil {
		t.Fatal(err)
	}
	release()
	waitForResult(t, w, first.TaskID)
	if _, err := os.Stat(filepath.Join(w.home, "notes.md")); err == nil {
		t.Fatal("the held parent's directory was written before its next turn")
	}
	// The next turn is open and holds the canonical lock.
	if _, err := w.attempts.Open(t.Context(), attempt.Spec{TaskID: parent.ID, Kind: attempt.KindChat, Project: "p", Agent: "codex", Harness: "mock",
		Workspace: project.Workspace{ID: "canonical:p", Project: "p", Path: w.home, Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted}); err != nil {
		t.Fatal(err)
	}
	text, _ := w.service.Preface(t.Context(), parent.ID)
	if !strings.Contains(text, "已落地") || strings.Contains(text, "排队中") {
		t.Fatalf("preface must say the files are in place:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(w.home, "notes.md")); err != nil {
		t.Fatalf("the child's file did not reach the main directory: %v", err)
	}
	if box.count() != 0 {
		t.Fatalf("delivered %d time(s) besides the preface", box.count())
	}
}

func waitForResult(t *testing.T, w *world, childID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := w.tasks.Get(childID); c.Finished() && c.Result != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
