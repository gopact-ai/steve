package agentmcp

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
)

// A restart may find the messaging port taken and listen elsewhere. The
// agent is given the new address, and its session keeps both fingerprints,
// whether the agent runs beside the gateway or reaches it through a node.
func TestMovedMessagingPortKeepsSessionFingerprints(t *testing.T) {
	first, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.listener.Close()
	moved, err := New(first.Port())
	if err != nil {
		t.Fatal(err)
	}
	defer moved.listener.Close()
	if moved.Port() == first.Port() {
		t.Fatalf("fixture did not move the port %d", first.Port())
	}
	assembler := capability.NewAssembler(nil)
	for _, tc := range []struct {
		name          string
		before, after []capability.Extra
		url           string
	}{
		{"gateway", first.DescribeExtras("token", ""), moved.DescribeExtras("token", ""), moved.URL()},
		{"node", first.DescribeExtras("token", "http://127.0.0.1:20001/mcp"), first.DescribeExtras("token", "http://127.0.0.1:20002/mcp"), "http://127.0.0.1:20002/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := assembler.AssembleExtra(agent.Agent{ID: "agent"}, home.ModeNone, tc.before)
			if err != nil {
				t.Fatal(err)
			}
			after, err := assembler.AssembleExtra(agent.Agent{ID: "agent"}, home.ModeNone, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if before.Fingerprint != after.Fingerprint || before.SessionFingerprint != after.SessionFingerprint {
				t.Fatal("a moved messaging port changed the session fingerprints")
			}
			if len(after.MCPServers) != 1 || after.MCPServers[0].URL != tc.url {
				t.Fatalf("agent is given %+v, want %s", after.MCPServers, tc.url)
			}
		})
	}
}
