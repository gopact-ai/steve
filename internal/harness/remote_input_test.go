package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestManagedInputMissingReceiptNeverInventsNextSequence(t *testing.T) {
	base := NodeSessionContext{Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "original"}, CommandID: "original-input"}
	s := &managedSession{base: base, id: "ns_original", at: Placement{Node: "worker"}}
	prompts := 0
	s.transport = stopTransportFunc(func(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
		state := nodewire.SessionState{ID: req.ID, Binding: req.Binding, InputAccepted: 10, State: nodewire.SessionIdle}
		if req.Action == nodewire.SessionActionPrompt {
			prompts++
			state.Command = &nodewire.SessionCommand{ID: req.CommandID, InputSequence: req.InputSequence, State: nodewire.SessionCommandCompleted, Settled: true}
		}
		return state, nil
	})
	if _, _, err := s.Prompt(t.Context(), "old input must not run again", nil); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("missing receipt was not refused: %v", err)
	}
	if prompts != 0 {
		t.Fatalf("missing receipt allocated a fresh sequence and sent %d prompts", prompts)
	}
}

func TestManagedInputUsesOnlyMatchingNodeHintOrReceipt(t *testing.T) {
	for _, mode := range []string{"hint", "receipt", "wrong-binding", "wrong-session", "wrong-sequence", "other-command", "zero-receipt"} {
		t.Run(mode, func(t *testing.T) {
			base := NodeSessionContext{Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "original"}, CommandID: "original-input"}
			s := &managedSession{base: base, id: "ns_original", at: Placement{Node: "worker"}}
			prompts := 0
			s.transport = stopTransportFunc(func(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
				state := nodewire.SessionState{ID: req.ID, Binding: req.Binding, InputAccepted: 10, NextInputSequence: 11, State: nodewire.SessionIdle}
				if req.Action == nodewire.SessionActionPrompt {
					prompts++
					want := uint64(11)
					if mode == "receipt" {
						want = 10
					}
					if req.InputSequence != want {
						t.Errorf("input sequence %d != node receipt/hint %d", req.InputSequence, want)
					}
					state.Command = &nodewire.SessionCommand{ID: req.CommandID, InputSequence: req.InputSequence, State: nodewire.SessionCommandCompleted, Settled: true}
					return state, nil
				}
				switch mode {
				case "receipt", "other-command", "zero-receipt":
					state.Command = &nodewire.SessionCommand{ID: req.CommandID, InputSequence: 10}
					if mode == "other-command" {
						state.Command.ID = "other"
					}
					if mode == "zero-receipt" {
						state.Command.InputSequence = 0
					}
				case "wrong-binding":
					state.Binding.AttemptID = "other"
				case "wrong-session":
					state.ID = "ns_other"
				case "wrong-sequence":
					state.NextInputSequence = 12
				}
				return state, nil
			})
			_, _, err := s.Prompt(t.Context(), "input", nil)
			allowed := mode == "hint" || mode == "receipt"
			if allowed && (err != nil || prompts != 1) || !allowed && (err == nil || prompts != 0) {
				t.Fatalf("unverified node hint: prompts=%d err=%v", prompts, err)
			}
		})
	}
}
