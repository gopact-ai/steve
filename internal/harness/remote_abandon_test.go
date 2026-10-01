package harness

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestAbandonedCleanupDirectlyAbortsAndRequiresPhysicalExit(t *testing.T) {
	for _, changed := range []string{"", "command-only", "binding", "command", "harness"} {
		t.Run(changed, func(t *testing.T) {
			m, err := NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Stop()
			identity := NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "c", CoordinatorNodeID: "hub", CoordinatorEpoch: 2, WriterGeneration: 3}, Binding: nodewire.SessionBinding{NodeID: "worker", TaskID: "task", AttemptID: "attempt", TaskEpoch: 1, ExecutionEpoch: 1}, CommandID: "input"}
			m.SetNodeSessionBinder(func(ctx context.Context, _ Placement, _, _ string) (context.Context, error) {
				return WithNodeSession(ctx, identity), nil
			})
			calls := 0
			m.SetTransports(directKillTransport{call: func(ctx context.Context, node string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
				calls++
				if request.Action != nodewire.SessionActionAbort || request.Binding != identity.Binding || request.ID != "ns_original" || node != "worker" {
					t.Fatal("cleanup used observation or a different execution")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("cleanup has no deadline")
				}
				st := nodewire.SessionState{ID: request.ID, Harness: "mock", Binding: request.Binding, ProcessStopped: true, InputAccepted: 1, Command: &nodewire.SessionCommand{ID: "input", InputSequence: 1, Settled: true, State: nodewire.SessionCommandCompleted}}
				switch changed {
				case "command-only":
					st.ProcessStopped = false
				case "binding":
					st.Binding.AttemptID = "new"
				case "command":
					st.Command.ID = "new"
				case "harness":
					st.Harness = "other"
				}
				return st, nil
			}})
			for range 2 {
				_, err := m.AbortRetainedSession(t.Context(), Placement{Node: "worker", Harness: "mock"}, "ns_original", "/work")
				if changed == "" && err != nil {
					t.Fatal(err)
				}
				if changed != "" && err == nil {
					t.Fatal("cleanup accepted a receipt without exact exit evidence")
				}
			}
			if calls != 2 {
				t.Fatalf("cleanup reused an old result or never called: %d", calls)
			}
		})
	}
}
