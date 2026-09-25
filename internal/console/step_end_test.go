package console

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

// A step that ends within its throttle window, just before the line that
// follows it stops, leaves the update it ended with in what the line
// collected.
func TestFollowKeepsTheUpdateAStepEndedWith(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test", Member: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	model := readmodel.New(readmodel.Sources{Tasks: tasks})
	s := New(turntest.IdleCoordinator{}, "owner", model)
	work := newProcess()
	stop := s.follow(context.Background(), "console:test", work)
	defer stop()
	collected := func(id string) (consoleapi.StepProcess, bool) {
		work.mu.Lock()
		defer work.mu.Unlock()
		step, ok := work.steps[id]
		return step, ok
	}
	// Collection has begun once a step published after follow is seen.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		model.Publish(readmodel.Event{Kind: "step.progress", Conversation: "console:test", StepID: "ready", Progress: &consoleapi.Progress{}})
		if _, ok := collected("ready"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follow never collected a step")
		}
	}
	for _, reasoning := range []string{"compiling", "compiling… done"} {
		model.StepProgress(tracked.ID, "plan", "build", "builder", "node-a", view.Progress{Reasoning: reasoning})
	}
	model.StepEnded(tracked.ID, "plan", "build", "builder", "node-a", view.Progress{Reasoning: "compiling… done"})
	stop()
	if step, _ := collected("build"); step.Reasoning != "compiling… done" {
		t.Fatalf("collected %+v, want the update the step ended with", step)
	}
}
