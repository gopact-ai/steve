package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestInvalidKillReceiptsRetainAnAnsweredClassification(t *testing.T) {
	for _, open := range []bool{false, true} {
		for _, changed := range []string{"binding", "harness", "command", "exit"} {
			t.Run(map[bool]string{false: "session", true: "open"}[open]+"/"+changed, func(t *testing.T) {
				m, err := NewManager(nil)
				if err != nil {
					t.Fatal(err)
				}
				defer m.Stop()
				binding := NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "c", CoordinatorNodeID: "hub", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "attempt"}, CommandID: "input"}
				m.SetNodeSessionBinder(func(ctx context.Context, _ Placement, _, _ string) (context.Context, error) {
					return WithNodeSession(ctx, binding), nil
				})
				m.SetTransports(directKillTransport{call: func(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
					state := nodewire.SessionState{ID: req.ID, Harness: "mock", Binding: req.Binding, ProcessStopped: true, State: nodewire.SessionClosed}
					if open {
						state.ID = nodewire.SessionOpenID("c", "worker", "attempt", req.CommandID, "mock")
						state.OpenReceipt = &nodewire.SessionOpenReceipt{Action: req.Action, Authority: req.Authority, CommandID: req.CommandID}
					}
					switch changed {
					case "binding":
						state.Binding.AttemptID = "other"
					case "harness":
						state.Harness = "other"
					case "command":
						if open {
							state.OpenReceipt.CommandID = "other"
						} else {
							state.Command = &nodewire.SessionCommand{ID: "other"}
						}
					case "exit":
						state.ProcessStopped = false
					}
					return state, nil
				}})
				if open {
					_, err = m.KillNodeOpen(t.Context(), Placement{Node: "worker", Harness: "mock"}, "/work")
				} else {
					_, err = m.KillRetainedSession(t.Context(), Placement{Node: "worker", Harness: "mock"}, "ns_original", "/work")
				}
				var response interface{ SessionErrorCode() string }
				if !errors.As(err, &response) || response.SessionErrorCode() != "stop_unproven" {
					t.Fatalf("node replied but proof was classified as unanswered: %v", err)
				}
				if !errors.Is(err, ErrStopUnconfirmed) {
					t.Fatal("invalid proof lost unconfirmed stop cause")
				}
			})
		}
	}
}
