package roster

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type fakeNodes struct{ statuses []node.Status }

func (f fakeNodes) Statuses() []node.Status { return f.statuses }

func (f fakeNodes) EnsureConnected(context.Context) {}

func testRoster(t *testing.T, nodes []node.Status, hubCaps []string) *Roster {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":   {Harness: "mock", Default: true},
		"builder": {Harness: "mock", Node: "node-a", Requires: []string{"gpu"}},
		"shipper": {Harness: "mock", Node: "node-b", Requires: []string{"prod-cred"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(catalog)
	r.SetNodes(fakeNodes{statuses: nodes})
	r.SetHubCapabilities(hubCaps)
	return r
}

func up(name string, caps []string, harnesses ...nodewire.Harness) node.Status {
	if len(harnesses) == 0 {
		harnesses = []nodewire.Harness{{ID: "mock"}}
	}
	return node.Status{Name: name, Up: true, Advert: nodewire.Advert{
		Node: name, Capabilities: caps, Harnesses: harnesses,
	}}
}

func TestCandidatesMatchOnLiveCapabilities(t *testing.T) {
	r := testRoster(t, []node.Status{
		up("node-a", []string{"gpu", "build"}),
		up("node-b", []string{"prod-cred", "internal-net"}),
	}, []string{"basic"})

	gpu := r.Candidates(t.Context(), []string{"gpu"}, nil)
	if len(gpu) != 1 || gpu[0].Agent.ID != "builder" {
		t.Fatalf("gpu candidates = %v", names(gpu))
	}
	prod := r.Candidates(t.Context(), []string{"prod-cred"}, nil)
	if len(prod) != 1 || prod[0].Agent.ID != "shipper" {
		t.Fatalf("prod candidates = %v", names(prod))
	}
	// "any" is not a requirement; every eligible agent qualifies.
	if all := r.Candidates(t.Context(), []string{"any"}, nil); len(all) != 3 {
		t.Fatalf("unconstrained candidates = %v, want all three", names(all))
	}
	// Excluding an agent that already failed forces a different choice.
	if left := r.Candidates(t.Context(), []string{"gpu"}, []string{"builder"}); len(left) != 0 {
		t.Fatalf("excluded agent still offered: %v", names(left))
	}
}

// A node that is down takes its agents out of the running, and the roster
// can say so — the placement failure names a restartable cause.
func TestDownNodeRemovesItsAgentsAndExplainsWhy(t *testing.T) {
	r := testRoster(t, []node.Status{
		up("node-a", []string{"gpu"}),
		{Name: "node-b", Up: false, LastError: "connection refused"},
	}, nil)

	if got := r.Candidates(t.Context(), []string{"prod-cred"}, nil); len(got) != 0 {
		t.Fatalf("a down node still offered candidates: %v", names(got))
	}
	why := r.Explain(t.Context(), []string{"prod-cred"})
	if !strings.Contains(why, "node-b is down") || !strings.Contains(why, "connection refused") {
		t.Fatalf("explanation = %q, want the node and the cause", why)
	}
}

// The advert is the authority: a harness the node cannot start disqualifies
// its agents even though the hub's config lists it.
func TestMissingHarnessDisqualifies(t *testing.T) {
	r := testRoster(t, []node.Status{
		up("node-a", []string{"gpu"}, nodewire.Harness{ID: "mock", Missing: `"mock" not on this node's PATH`}),
	}, nil)
	if got := r.Candidates(t.Context(), []string{"gpu"}, nil); len(got) != 0 {
		t.Fatalf("an unusable harness still offered: %v", names(got))
	}
	if why := r.Explain(t.Context(), []string{"gpu"}); !strings.Contains(why, "PATH") {
		t.Fatalf("explanation = %q, want the harness reason", why)
	}
}

// A pinned model the node does not offer is caught at placement, not at the
// first prompt.
func TestPinnedModelMustBeOffered(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"picky": {Harness: "mock", Node: "node-a", Model: "gpt-9", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(catalog)
	r.SetNodes(fakeNodes{statuses: []node.Status{
		up("node-a", []string{"gpu"}, nodewire.Harness{ID: "mock", Models: []string{"gpt-5"}}),
	}})
	if got := r.Candidates(t.Context(), nil, nil); len(got) != 0 {
		t.Fatalf("agent with an unavailable model was offered: %v", names(got))
	}
	if why := r.Explain(t.Context(), nil); !strings.Contains(why, "gpt-9") {
		t.Fatalf("explanation = %q, want the model named", why)
	}
}

// Hub-local agents are not exempt from capability matching: "here" is not a
// reason to run work that needs hardware this machine lacks.
func TestHubIsNotExemptFromCapabilities(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local": {Harness: "mock", Requires: []string{"gpu"}, Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(catalog)
	r.SetHubCapabilities([]string{"basic"})
	if got := r.Candidates(t.Context(), nil, nil); len(got) != 0 {
		t.Fatalf("hub ran work it lacks the capability for: %v", names(got))
	}
	r.SetHubCapabilities([]string{"basic", "gpu"})
	if got := r.Candidates(t.Context(), nil, nil); len(got) != 1 {
		t.Fatalf("hub with the capability was still excluded: %v", names(got))
	}
}

func names(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Agent.ID
	}
	return out
}
