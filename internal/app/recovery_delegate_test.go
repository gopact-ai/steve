package app

import (
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// A delegated child runs on a node-owned session under its own attempt kind.
// Its accounting is settled by the delegate service, never by a conversation
// continuation, so startup must neither refuse to start over it nor treat it
// as an interrupted chat.
func TestStartupRecoveryLeavesRunningDelegateChildToItsOwner(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	parent := first.seed(t, false, consoleapi.ExchangeRunning)
	child, err := first.tasks.Spawn(parent.TaskID, task.Task{Goal: "child goal", Member: "builder", Node: "node-b", Origin: "delegate:" + parent.TaskID, ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.tasks.Begin(child.ID, "builder", "node-b", ""); err != nil {
		t.Fatal(err)
	}
	token, err := first.tasks.ExecutionToken(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := first.attempts.Open(first.ctx, attempt.Spec{ID: "child-attempt", TaskID: child.ID, TurnID: "delegate/" + child.ID, Kind: attempt.KindDelegate, Agent: "builder", Node: "node-b", Harness: "test", Project: "p", Workspace: project.Workspace{ID: "wt", Project: "p", Node: "node-b", Path: t.TempDir(), Kind: project.KindWorktree}, Scope: attempt.ScopePathSet, Execution: &token})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := first.attempts.Advance(first.ctx, r.ID, phase, "test", func(r *attempt.Record) { r.Session = "ns_child" }); err != nil {
			t.Fatal(err)
		}
	}
	first.close(t)
	second := openCrashProbe(t, dir)
	defer second.close(t)
	if err := second.assemble(t); err != nil {
		t.Fatalf("startup refused a running delegated child: %v", err)
	}
	got, _ := second.tasks.Get(child.ID)
	if got.State != task.StateRunning || !got.HasOpenExecution() || second.calls.Load() != 0 {
		t.Fatalf("delegated child was settled or replayed by chat recovery: %+v calls=%d", got, second.calls.Load())
	}
	if record, err := second.attempts.Get(second.ctx, r.ID); err != nil || record.State != attempt.Running || record.Session != "ns_child" {
		t.Fatalf("delegated child's retained execution was not left to the delegate service: %+v %v", record, err)
	}
	for _, e := range second.cons.Queue(crashConversation) {
		if e.ExpectedTask == child.ID {
			t.Fatalf("delegated child was queued as a conversation continuation: %+v", e)
		}
	}
}
