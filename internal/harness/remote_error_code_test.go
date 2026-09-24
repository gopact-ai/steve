package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// A node's classification of a command's error reaches the hub as the
// error's identity, with the node's message kept as its text. The message
// is never classified in its place.
func TestManagedPromptErrorCarriesTheNodeErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   nodewire.SessionCommand
		code      string
		uncertain bool
	}{
		{"coded deadline", nodewire.SessionCommand{State: nodewire.SessionCommandCompleted, Settled: true, Error: "prompt timed out", ErrorCode: nodewire.SessionErrorDeadline}, nodewire.SessionErrorDeadline, false},
		{"coded failure mentioning a deadline", nodewire.SessionCommand{State: nodewire.SessionCommandCompleted, Settled: true, Error: "tool: context deadline exceeded", ErrorCode: nodewire.SessionErrorFailed}, nodewire.SessionErrorFailed, false},
		{"uncoded message mentioning a deadline", nodewire.SessionCommand{State: nodewire.SessionCommandCompleted, Settled: true, Error: "prompt: context deadline exceeded"}, "", false},
		{"uncertain cancel", nodewire.SessionCommand{State: nodewire.SessionCommandUncertain, Error: "prompt interrupted", ErrorCode: nodewire.SessionErrorCanceled}, nodewire.SessionErrorCanceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := NodeSessionContext{Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "original"}, CommandID: "original-input"}
			s := &managedSession{base: base, id: "ns_original", at: Placement{Node: "worker"}}
			s.transport = stopTransportFunc(func(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
				state := nodewire.SessionState{ID: req.ID, Binding: req.Binding, InputAccepted: 10, NextInputSequence: 11, State: nodewire.SessionIdle}
				if req.Action == nodewire.SessionActionPrompt {
					command := tc.command
					command.ID, command.InputSequence = req.CommandID, req.InputSequence
					state.Command = &command
				}
				return state, nil
			})
			_, _, err := s.Prompt(t.Context(), "input", nil)
			if err == nil || RemoteErrorCode(err) != tc.code || errors.Is(err, ErrStopUnconfirmed) != tc.uncertain {
				t.Fatalf("err = %v, code %q, want code %q (uncertain %v)", err, RemoteErrorCode(err), tc.code, tc.uncertain)
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("a remote error passed for a local context error: %v", err)
			}
		})
	}
}
