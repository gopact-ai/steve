package attempt

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

func TestChatOpenCannotAdmitAnAlreadyEndedTurn(t *testing.T) {
	s := identityStore(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = tasks.BeginTurn(tracked.ID, "worker", "", task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	// The task epoch remains valid. Ended turn accounting, not a global task
	// cancellation, must fence delayed admission of this particular input.
	if err := tasks.CheckExecution(token); err != nil {
		t.Fatal(err)
	}
	_, err = s.Open(t.Context(), Spec{ID: "late", TaskID: tracked.ID, TurnID: "original", Execution: &token, Kind: KindChat, Scope: ScopeNone})
	if !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("ended input admitted a delayed chat execution: %v", err)
	}
	if _, found, err := s.LatestForTurn(t.Context(), "original"); err != nil || found {
		t.Fatalf("refused chat wrote an attempt: found=%v err=%v", found, err)
	}
}

func TestChatOpenRechecksEndedAccountingAfterLeaseAcquisition(t *testing.T) {
	s := identityStore(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "", task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "original"}}); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s.l.RegisterIssuer("remote", &fakeIssuer{onAcquire: func(context.Context) error {
		close(entered)
		<-release
		return nil
	}})
	result := make(chan error, 1)
	go func() {
		_, err := s.Open(t.Context(), Spec{ID: "late", TaskID: tracked.ID, TurnID: "original", Execution: &token, Kind: KindChat, Scope: ScopePathSet, Region: "remote", Project: "p", Workspace: worktree("late", "p")})
		result <- err
	}()
	<-entered
	if _, err := tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0); err != nil {
		close(release)
		t.Fatal(err)
	}
	_, confirmed, proofErr := s.UnadmittedTurn(t.Context(), channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "original"})
	close(release)
	if err := <-result; !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("delayed Open escaped accounting fence: %v", err)
	}
	if proofErr != nil || !confirmed {
		t.Fatalf("ended unadmitted input proof=%v err=%v", confirmed, proofErr)
	}
	// Fencing one ended input must not cancel its task or block a genuinely
	// new input on the same task epoch.
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "", task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "next"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "next", TaskID: tracked.ID, TurnID: "next", Execution: &token, Kind: KindChat, Scope: ScopeNone}); err != nil {
		t.Fatalf("new input inherited old input's fence: %v", err)
	}
}

func TestTaskBackedChatCannotBypassAdmissionFenceByOmittingExecutionToken(t *testing.T) {
	for _, accounting := range []string{"open", "ended"} {
		t.Run(accounting, func(t *testing.T) {
			s := identityStore(t)
			tasks, err := task.OpenLedger(s.l, "")
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test", Member: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if accounting != "none" {
				if _, err := tasks.BeginTurn(tracked.ID, "worker", "", task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "original"}}); err != nil {
					t.Fatal(err)
				}
			}
			if accounting == "ended" {
				if _, err := tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Open(t.Context(), Spec{ID: "no-token", TaskID: tracked.ID, TurnID: "original", Kind: KindChat, Scope: ScopeNone}); !errors.Is(err, task.ErrExecutionStopped) {
				t.Fatalf("task-backed chat admitted without its original execution authority: %v", err)
			}
		})
	}
}

func TestUntrackedChatAdmissionStillWorksWithoutExecutionToken(t *testing.T) {
	s := identityStore(t)
	if _, err := s.Open(t.Context(), Spec{ID: "direct", TaskID: "untracked", TurnID: "input", Kind: KindChat, Scope: ScopeNone}); err != nil {
		t.Fatalf("untracked chat consumer requires nonexistent task token: %v", err)
	}
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "without-accounting", TaskID: tracked.ID, Kind: KindChat, Scope: ScopeNone}); err != nil {
		t.Fatalf("direct consumer with task header but no accounting regressed: %v", err)
	}
}

func TestChatCannotReadmitTheSameInputAfterNeverAdmittedProof(t *testing.T) {
	s := identityStore(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:test", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	input := task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: "original"}}
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "", input); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish(tracked.ID, task.OutcomeError, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, yes, err := s.UnadmittedTurn(t.Context(), input.Address); err != nil || !yes {
		t.Fatalf("initial proof=%v err=%v", yes, err)
	}
	// A repeated message cannot supersede its positive ended-input receipt,
	// even if some caller erroneously charged another task accounting row.
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "", input); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "replay", TaskID: tracked.ID, TurnID: "original", Execution: &token, Kind: KindChat, Scope: ScopeNone}); err == nil {
		t.Fatal("same input bypassed its ended-row fence")
	}
}
