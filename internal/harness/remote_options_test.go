package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

type optionNodeTransport struct {
	request nodewire.SessionRequest
}

func (*optionNodeTransport) Transport(string, string) acphost.Transport { return nil }

func (tr *optionNodeTransport) NodeSession(_ context.Context, _ string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	// The hub's string option request must survive its node JSON boundary.
	raw, err := json.Marshal(request)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if err := json.Unmarshal(raw, &tr.request); err != nil {
		return nodewire.SessionState{}, err
	}
	return nodewire.SessionState{
		Sequence: 1,
		Settings: view.Settings{Options: []view.Option{{ID: "toggle", Type: "boolean", Current: "true"}}},
	}, nil
}

func TestBooleanManagedOptionKeepsAuthorityAndUsesReturnedActual(t *testing.T) {
	transport := &optionNodeTransport{}
	binding := nodewire.SessionBinding{NodeID: "node", AttemptID: "attempt", ExecutionEpoch: 3}
	base := NodeSessionContext{Binding: binding, CommandID: "command", InputSequence: 2}
	session := &managedSession{transport: transport, id: "native", at: Placement{Node: "node"}, base: base}
	if err := session.SetOption(t.Context(), "toggle", "false"); err != nil {
		t.Fatal(err)
	}
	request := transport.request
	if request.Action != nodewire.SessionActionOption || request.OptionID != "toggle" || request.OptionValue != "false" ||
		request.ID != "native" || request.CommandID != base.CommandID || request.InputSequence != base.InputSequence || !reflect.DeepEqual(request.Binding, binding) {
		t.Fatalf("boolean request changed scope or lost false: %+v", request)
	}
	if request.Permission != "" {
		t.Fatal("an option request tried to grant a new permission policy")
	}
	actual := session.Settings().Options[0]
	if actual.Type != "boolean" || actual.Current != "true" {
		t.Fatalf("requested false replaced the node's corrected Actual: %+v", actual)
	}
}
