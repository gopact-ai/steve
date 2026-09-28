package turn

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/state"
)

// platformGate marks its server as Steve's own messaging server, the way
// the real gate does, so fingerprints leave out the port it listens on.
type platformGate struct{ *fakeGate }

func (g platformGate) PrepareExtras(conversationID, agentID, token, endpoint string) ([]capability.Extra, error) {
	extras, err := g.fakeGate.PrepareExtras(conversationID, agentID, token, endpoint)
	return markPlatform(extras), err
}

func (g platformGate) DescribeExtras(token, endpoint string) []capability.Extra {
	return markPlatform(g.fakeGate.DescribeExtras(token, endpoint))
}

func markPlatform(extras []capability.Extra) []capability.Extra {
	for i := range extras {
		extras[i].Platform = true
	}
	return extras
}

// The setup page says a session already read its instructions once a turn
// gave them to it, whether or not the agent was also given the messaging
// server, and wherever the agent runs. Reading it prepares no credential.
func TestSessionSetupIsAppliedAfterATurnReadTheInstructions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mcpHTTP bool
		agent   string
		input   []string
	}{
		{name: "without messaging", agent: "codex", input: []string{"hello"}},
		{name: "messaging on the hub", mcpHTTP: true, agent: "codex", input: []string{"hello"}},
		{name: "messaging on a node", mcpHTTP: true, agent: "lab", input: []string{"/project use lab", "@lab go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := agent.NewCatalog(map[string]agent.Config{
				"codex": {Harness: "codex", Default: true},
				"lab":   {Harness: "codex", Node: "host-3", Aliases: []string{"lab"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.OpenLedger(testLedger(t))
			if err != nil {
				t.Fatal(err)
			}
			gate := &fakeGate{}
			manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}, mcpHTTP: tc.mcpHTTP}
			coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute,
				withDeps(func(d *Deps) { d.Nodes = fakeEndpoints{port: map[string]int{"host-3": 45999}} }),
				withCallbacks(func(cb *Callbacks) { cb.AgentGate = platformGate{gate} }))
			for _, input := range tc.input {
				if _, err := handle(coordinator, t.Context(), input); err != nil {
					t.Fatal(err)
				}
			}
			saved := store.Conversation("chat").Sessions[tc.agent]
			if !saved.InstructionsApplied || (saved.AgentToken != "") != tc.mcpHTTP {
				t.Fatalf("the turn left applied=%v token=%q", saved.InstructionsApplied, saved.AgentToken)
			}
			prepared := len(gate.calls)
			setup, err := coordinator.SessionSetup(t.Context(), "chat", tc.agent)
			if err != nil {
				t.Fatal(err)
			}
			if len(gate.calls) != prepared {
				t.Fatal("reading the setup prepared a messaging credential")
			}
			if !setup.Applied {
				t.Fatal("setup reports the instructions not yet applied after a turn gave them to the session")
			}
		})
	}
}
