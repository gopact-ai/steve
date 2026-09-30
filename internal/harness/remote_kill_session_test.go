package harness

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type directKillTransport struct {
	call func(context.Context, string, nodewire.SessionRequest) (nodewire.SessionState, error)
}

func (t directKillTransport) Transport(string, string) acphost.Transport { return nil }
func (t directKillTransport) NodeSession(ctx context.Context, node string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return t.call(ctx, node, req)
}

func TestDirectKillUsesOriginalBindingAndRejectsInvalidReceipts(t *testing.T) {
	for _, changed := range []string{"", "id", "binding", "harness", "command", "not-stopped", "placement"} {
		t.Run(changed, func(t *testing.T) {
			m, err := NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Stop()
			binding := NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "c", CoordinatorNodeID: "hub", CoordinatorEpoch: 3, WriterGeneration: 4}, Binding: nodewire.SessionBinding{NodeID: "worker", TaskID: "task", AttemptID: "attempt", TaskEpoch: 2, ExecutionEpoch: 1}, CommandID: "input"}
			calls := 0
			m.SetNodeSessionBinder(func(ctx context.Context, at Placement, id, path string) (context.Context, error) {
				if at.Node != "worker" || at.Harness != "mock" || id != "ns_original" || path != "/work" {
					t.Fatal("binder lost original placement")
				}
				return WithNodeSession(ctx, binding), nil
			})
			m.SetTransports(directKillTransport{call: func(ctx context.Context, node string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
				calls++
				if req.Action != nodewire.SessionActionKill || node != "worker" || req.ID != "ns_original" || req.Binding != binding.Binding || req.Authority != binding.Authority || req.CommandID != binding.CommandID {
					t.Fatal("kill changed original identity or used observation")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("kill has no deadline")
				}
				state := nodewire.SessionState{ID: req.ID, Harness: "mock", Binding: req.Binding, ProcessStopped: true, InputAccepted: 1, Command: &nodewire.SessionCommand{ID: "input", InputSequence: 1}}
				switch changed {
				case "id":
					state.ID = "ns_other"
				case "binding":
					state.Binding.AttemptID = "other"
				case "harness":
					state.Harness = "other"
				case "command":
					state.Command.ID = "other"
				case "not-stopped":
					state.ProcessStopped = false
				}
				return state, nil
			}})
			if changed == "placement" {
				binding.Binding.NodeID = "another"
			}
			_, err = m.KillRetainedSession(t.Context(), Placement{Node: "worker", Harness: "mock"}, "ns_original", "/work")
			if changed == "" && err != nil {
				t.Fatal(err)
			}
			if changed != "" && err == nil {
				t.Fatal("kill accepted an invalid receipt or placement")
			}
			want := 1
			if changed == "placement" {
				want = 0
			}
			if calls != want {
				t.Fatalf("RPC count=%d want=%d", calls, want)
			}
		})
	}
}
