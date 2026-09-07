package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type stopTransportFunc func(context.Context, string, nodewire.SessionRequest) (nodewire.SessionState, error)

func (f stopTransportFunc) NodeSession(ctx context.Context, node string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	return f(ctx, node, request)
}

func TestManagedStopRequiresNativeReceiptAndFallsBackToActualProcessExit(t *testing.T) {
	for _, mode := range []string{"settled", "abort-stopped", "abort-unknown", "wrong-binding", "wrong-command", "disconnected"} {
		t.Run(mode, func(t *testing.T) {
			binding := NodeSessionContext{Binding: nodewire.SessionBinding{TaskID: "task", AttemptID: "attempt", NodeID: "worker"}, CommandID: "command"}
			s := &managedSession{base: binding, id: "ns_original", at: Placement{Node: "worker"}}
			var actions []string
			s.transport = stopTransportFunc(func(ctx context.Context, _ string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
				actions = append(actions, request.Action)
				if _, ok := ctx.Deadline(); !ok {
					t.Error("native stop RPC has no deadline")
				}
				state := nodewire.SessionState{ID: request.ID, Binding: request.Binding, State: "running", Sequence: uint64(len(actions)), Command: &nodewire.SessionCommand{ID: request.CommandID, State: "running"}}
				switch mode {
				case "settled":
					state.Command.State, state.Command.Settled = "cancelled", true
				case "abort-stopped":
					state.ProcessStopped = request.Action == "abort"
					state.Command.ProcessStopped = state.ProcessStopped
				case "wrong-binding":
					state.Binding.AttemptID = "other"
					state.ProcessStopped, state.Command.Settled = true, true
				case "wrong-command":
					state.Command.ID, state.Command.Settled = "other", true
				case "disconnected":
					return nodewire.SessionState{}, errors.New("node disconnected")
				}
				return state, nil
			})
			err := s.stopExecution(t.Context())
			confirmed := mode == "settled" || mode == "abort-stopped"
			if confirmed && err != nil || !confirmed && !errors.Is(err, ErrStopUnconfirmed) {
				t.Fatalf("stop evidence classification: %v", err)
			}
			want := []string{"cancel", "abort"}
			if mode == "settled" {
				want = []string{"cancel"}
			}
			if !reflect.DeepEqual(actions, want) {
				t.Fatalf("stop RPC sequence: %v", actions)
			}
			_ = s.stopExecution(t.Context())
			if !reflect.DeepEqual(actions, want) {
				t.Fatalf("duplicate stop invoked native operations again: %v", actions)
			}
			output, activity, result := "", []string(nil), error(ErrStopUnconfirmed)
			s.reconcileStop(s.request(t.Context(), "poll"), &output, &activity, &result)
			if acphost.PromptSettled(result) != confirmed {
				t.Fatalf("observation replaced real stop evidence: %v", result)
			}
		})
	}
}

func TestManagedStopDoesNotMaskAnUnrelatedPromptError(t *testing.T) {
	s := &managedSession{id: "ns_original"}
	failure := errors.New("native input receipt could not be persisted")
	output, activity, result := "partial", []string{"one"}, failure
	s.reconcileStop(s.request(t.Context(), "poll"), &output, &activity, &result)
	if !errors.Is(result, failure) || output != "partial" || len(activity) != 1 {
		t.Fatal("stop masked unrelated failure or output")
	}
}
