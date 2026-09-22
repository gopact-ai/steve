package nodewire

import (
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func receiptState() SessionState {
	return SessionState{ID: "native-session", ContextID: "original-context", Binding: SessionBinding{
		NodeID: "worker", ProjectID: "project", TaskID: "task", AttemptID: "attempt", SessionID: "conversation", ExecutionEpoch: 1, TaskEpoch: 2},
		Command:  &SessionCommand{ID: "command", InputSequence: 7, State: SessionCommandCompleted, Settled: true, Output: "answer", DispatchState: "dispatched"},
		Progress: view.Progress{Usage: view.Usage{Reported: true, InputTokens: 5}}}
}

func TestNodeReceiptIdentityBindsOnlyImmutableTerminalEvidence(t *testing.T) {
	state := receiptState()
	receipt, err := NewSessionReceipt(state)
	if err != nil {
		t.Fatal(err)
	}
	changed := state
	changed.Sequence = 900
	changed.InputAccepted = 100
	changed.NextInputSequence = 101
	changed.State = SessionClosed
	changed.ProcessStopped = true
	command := *state.Command
	command.ProcessStopped, command.CancelRequested = true, true
	changed.Command = &command
	same, err := NewSessionReceipt(changed)
	if err != nil || same != receipt {
		t.Fatalf("cleanup changed terminal receipt: %+v %v", same, err)
	}
	for _, mutate := range []func(*SessionState){
		func(s *SessionState) { s.ID = "other-session" },
		func(s *SessionState) { s.ContextID = "other-context" },
		func(s *SessionState) { s.Binding.AttemptID = "rebound" },
		func(s *SessionState) { s.Command.InputSequence++ },
		func(s *SessionState) { s.Command.ID = "other-command" },
		func(s *SessionState) { s.Command.Output = "other answer" },
		func(s *SessionState) { s.Command.Error = "failed" },
		func(s *SessionState) { s.Progress.Usage.InputTokens++ },
	} {
		changed := receiptState()
		mutate(&changed)
		got, err := NewSessionReceipt(changed)
		if err != nil || got == receipt {
			t.Fatalf("different evidence shared a receipt: %+v %v", got, err)
		}
	}
}

func TestNodeReceiptNeverRepresentsUnknownActiveOrPendingEvidence(t *testing.T) {
	for _, mutate := range []func(*SessionState){
		func(s *SessionState) { s.Command = nil },
		func(s *SessionState) { s.Command.InputSequence = 0 },
		func(s *SessionState) { s.Command.State = SessionCommandRunning },
		func(s *SessionState) { s.Command.State = SessionCommandUncertain },
		func(s *SessionState) { s.Command.Settled = false },
		func(s *SessionState) { s.Binding.AttemptID = "" },
		func(s *SessionState) {
			s.Questions = []SessionQuestion{{ID: "question", CommandID: "command", State: SessionQuestionPending}}
		},
		func(s *SessionState) {
			s.Questions = []SessionQuestion{{ID: "question", CommandID: "other-command", State: SessionQuestionAnswered}}
		},
	} {
		state := receiptState()
		mutate(&state)
		if _, err := NewSessionReceipt(state); err == nil {
			t.Fatal("nonterminal or incomplete evidence acquired a receipt")
		}
	}
}
