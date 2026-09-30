package turn

import (
	"context"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"testing"
)

func TestForceStopAttemptRequiresOwnerAndCancelsTask(t *testing.T) {
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	tracked, err := tasks.Create(task.Task{Channel: "console:original", Transport: "console", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "force-attempt", Kind: attempt.KindChat, TaskID: tracked.ID, TurnID: "original-input", Execution: &token, Project: "p", Node: "node", Harness: "mock", Agent: "worker", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original", Project: "p", Path: t.TempDir(), Node: "node", Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_original" })
		if err != nil {
			t.Fatal(err)
		}
	}
	force, ok := any(c).(interface {
		ForceStopAttempt(context.Context, string, string) error
	})
	if !ok {
		t.Fatal("coordinator has no force stop entry")
	}
	if err := force.ForceStopAttempt(t.Context(), r.ID, "visitor"); err == nil {
		t.Fatal("non-owner force stopped a task")
	}
	before, _ := tasks.Get(tracked.ID)
	if before.State != task.StateRunning {
		t.Fatal("unauthorized request changed task")
	}
	if err := force.ForceStopAttempt(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.Get(tracked.ID)
	got, _ := c.attempts.Get(t.Context(), r.ID)
	if after.State != task.StateCancelled || got.ForceStop == nil || got.ForceStop.By != "owner" {
		t.Fatalf("intent not durable: task=%s attempt=%+v", after.State, got)
	}
	epoch := after.ExecutionEpoch
	if err := force.ForceStopAttempt(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	again, _ := tasks.Get(tracked.ID)
	got, _ = c.attempts.Get(t.Context(), r.ID)
	if again.ExecutionEpoch != epoch || got.ForceStop.Revision != 2 {
		t.Fatal("retry re-revoked task or lost revision")
	}
}
