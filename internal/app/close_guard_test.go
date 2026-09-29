package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// keepSession gives the worker a session in the conversation, the one a
// reset archives. It lives on the hub, so closing it needs no node.
func (f cancelledRecovery) keepSession(t *testing.T) {
	t.Helper()
	if err := f.sessions.SaveSession(state.Session{ConversationID: cancelledRecoveryConversation, AgentID: "worker", HarnessID: "test", UpstreamID: "hub-session"}); err != nil {
		t.Fatal(err)
	}
}

// command runs input as the owner's console command, the way the console
// hands it to the coordinator.
func (f cancelledRecovery) command(t *testing.T, exchange, input string) (turn.Result, error) {
	t.Helper()
	return f.coordinator.Handle(t.Context(), turn.Request{
		Channel: "console", ConversationID: cancelledRecoveryConversation, ChatID: console.ChatID,
		MessageID: console.AnchorMark + exchange, ExchangeID: exchange, Input: input,
		SenderOpenID: "owner", ChatType: protocol.ChatP2P, Mentioned: true, Locale: "en",
	})
}

// A reset or a project switch ends the conversation's task as done, and a
// task whose original execution still waits on the owner is not done: the
// continuation would carry on under a task that says it finished. Either is
// refused before anything changes, and the refusal says how to get out.
func TestSessionResetKeepsATaskWhoseContinuationIsUnsettled(t *testing.T) {
	for name, input := range map[string]string{"reset": "/new", "switch": "/project use p2"} {
		t.Run(name, func(t *testing.T) {
			f := openCancelledRecovery(t)
			f.awaitOffering(t, "retry")
			f.keepSession(t)
			result, err := f.command(t, "e2", input)
			var refusal turn.UserError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, string(protocol.CommandTasks)+" cancel "+f.taskID) {
				t.Fatalf("%s with an unsettled continuation = %+v, %v; want a refusal naming %s cancel %s", input, result, err, protocol.CommandTasks, f.taskID)
			}
			if tracked, _ := f.tasks.Get(f.taskID); tracked.State != task.StateRunning {
				t.Fatalf("task after the refused %s = %s, want running", input, tracked.State)
			}
			if _, kept := f.sessions.Conversation(cancelledRecoveryConversation).Sessions["worker"]; !kept {
				t.Fatalf("refused %s archived the session", input)
			}
			if binding, _, err := f.projects.Binding(t.Context(), cancelledRecoveryConversation); err != nil || binding.ProjectID == "p2" {
				t.Fatalf("refused %s left the conversation bound to %+v, %v", input, binding, err)
			}
		})
	}
}

// The way out of a refused reset or switch is to cancel the task, and it
// holds while the stop of the task's original execution cannot be confirmed
// because the node holding it is away: a cancelled task is no longer the
// conversation's, so there is nothing left for the reset to end.
func TestSessionResetGoesAheadOnceTheTaskIsCancelled(t *testing.T) {
	for name, input := range map[string]string{"reset": "/new", "switch": "/project use p2"} {
		t.Run(name, func(t *testing.T) {
			f := openCancelledRecovery(t)
			f.awaitOffering(t, "retry")
			if _, err := f.cons.SendCommand(t.Context(), cancelledRecoveryConversation, "/tasks cancel "+f.taskID, "cancel-1"); err != nil {
				t.Fatal(err)
			}
			f.awaitOffering(t, "recheck")
			if e := f.exchange(); e.State != consoleapi.ExchangeAwaitingUser {
				t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
			}
			f.keepSession(t)
			if result, err := f.command(t, "e3", input); err != nil {
				t.Fatalf("%s after the task was cancelled = %+v, %v; want it to go ahead", input, result, err)
			}
			if tracked, _ := f.tasks.Get(f.taskID); tracked.State != task.StateCancelled {
				t.Fatalf("task after %s = %s, want cancelled", input, tracked.State)
			}
			if _, kept := f.sessions.Conversation(cancelledRecoveryConversation).Sessions["worker"]; kept {
				t.Fatalf("%s after the task was cancelled kept the session", input)
			}
		})
	}
}

// A schedule's firing rotates the task its last run opened, and a run still
// waiting on the owner has not ended: the task stays with it.
func TestScheduleRotationKeepsATaskWhoseContinuationIsUnsettled(t *testing.T) {
	f := openRetainedRecovery(t, "schedule:1")
	f.awaitOffering(t, "retry")
	f.coordinator.RotateTask(cancelledRecoveryConversation, "worker", "schedule:1")
	if tracked, _ := f.tasks.Get(f.taskID); tracked.State != task.StateRunning {
		t.Fatalf("scheduled task rotated while its run waits on the owner: %s, want running", tracked.State)
	}
}
