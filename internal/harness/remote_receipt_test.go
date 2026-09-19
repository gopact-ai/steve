package harness

import (
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestManagedReceiptOnlyReturnsMatchingAuthenticatedState(t *testing.T) {
	binding := nodewire.SessionBinding{NodeID: "node", ProjectID: "project", TaskID: "task", AttemptID: "attempt",
		SessionID: "conversation", ExecutionEpoch: 1, TaskEpoch: 1}
	base := NodeSessionContext{Binding: binding, CommandID: "command"}
	state := nodewire.SessionState{ID: "native", ContextID: "context", Binding: binding,
		Command: &nodewire.SessionCommand{ID: "command", InputSequence: 3, State: nodewire.SessionCommandCompleted, Settled: true}}
	receipt, err := nodewire.NewSessionReceipt(state)
	if err != nil {
		t.Fatal(err)
	}
	state.Command.Receipt = receipt
	for _, mode := range []string{"exact", "wrong-session", "wrong-context", "wrong-binding", "wrong-command", "wrong-sequence", "unknown", "no-node-receipt"} {
		t.Run(mode, func(t *testing.T) {
			s := &managedSession{base: base, id: state.ID, state: state}
			command := *state.Command
			s.state.Command = &command
			switch mode {
			case "wrong-session":
				s.state.ID = "another"
			case "wrong-context":
				s.state.ContextID = "another"
			case "wrong-binding":
				s.state.Binding.AttemptID = "rebound"
			case "wrong-command":
				command.ID = "other"
			case "wrong-sequence":
				command.InputSequence++
			case "unknown":
				command.Settled = false
			case "no-node-receipt":
				command.Receipt = nodewire.SessionReceipt{}
			}
			got, found := s.NodeReceipt(t.Context())
			if mode == "exact" {
				if !found || got != receipt {
					t.Fatalf("missing exact node receipt: %+v %t", got, found)
				}
			} else if found {
				t.Fatalf("unrelated client state manufactured receipt: %+v", got)
			}
		})
	}
}
