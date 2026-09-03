package roster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/models"
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

// A blocked agent is repairable only when its harness's binary is missing
// on a machine that is up and has another healthy agent; the helper is the
// one best at installing things. Everything else says why not.
func TestRepairPicksAHelperOnTheSameMachine(t *testing.T) {
	catalog := mustCatalog(t, map[string]agent.Config{
		"kimi":    {Harness: "kimi", Node: "node-a"},
		"builder": {Harness: "codex", Node: "node-a"},
		"fixer":   {Harness: "claude-code", Node: "node-a"},
		"shipper": {Harness: "codex", Node: "node-b"},
		"ghost":   {Harness: "codex", Node: "node-c"},
		"local":   {Harness: "kimi", Default: true},
	})
	r := New(catalog)
	r.SetNodes(fakeNodes{statuses: []node.Status{
		{Name: "node-a", Up: true, Advert: nodewire.Advert{Harnesses: []nodewire.Harness{
			{ID: "kimi", Command: "kimi", Missing: `"kimi" not on this node's PATH`},
			{ID: "codex", Command: "codex"},
			{ID: "claude-code", Command: "claude-code-acp"},
		}}},
		{Name: "node-b", Up: true, Advert: nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "codex", Command: "codex"}}}},
		{Name: "node-c", Up: false, LastError: "connection refused"},
	}})
	r.SetHubAdvert(func() nodewire.Advert {
		return nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "kimi", Command: "kimi", Missing: `"kimi" not on this node's PATH`}}}
	})

	fix, err := r.Repair(t.Context(), "kimi")
	if err != nil {
		t.Fatal(err)
	}
	if fix.Helper.Agent.ID != "fixer" || fix.Command != "kimi" || fix.Broken.Missing == "" {
		t.Fatalf("fix = helper %s command %q broken %+v", fix.Helper.Agent.ID, fix.Command, fix.Broken)
	}
	if _, err := r.Repair(t.Context(), "builder"); !errors.Is(err, ErrNotBroken) {
		t.Fatalf("repairing a healthy agent: %v", err)
	}
	if _, err := r.Repair(t.Context(), "ghost"); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("a node that is down is not repaired by running something on it: %v", err)
	}
	// The hub's own broken harness has nobody else on the hub to fix it.
	if _, err := r.Repair(t.Context(), "local"); err == nil || !strings.Contains(err.Error(), "no healthy agent") {
		t.Fatalf("hub without a helper: %v", err)
	}
	if _, err := r.Repair(t.Context(), "nobody"); err == nil {
		t.Fatal("unknown agent repaired")
	}
}

type fakeBook map[string]models.Observation

func (b fakeBook) Get(node, harness string) (models.Observation, bool) {
	o, ok := b[node+"/"+harness]
	return o, ok
}

// A model observed running fills in for one nobody declared; a pinned one
// still wins.
func TestObservedModelsFillTheRoster(t *testing.T) {
	catalog := mustCatalog(t, map[string]agent.Config{
		"codex":   {Harness: "codex", Default: true},
		"pinned":  {Harness: "codex", Model: "gpt-5-mini"},
		"builder": {Harness: "codex", Node: "node-a"},
	})
	r := New(catalog)
	r.SetNodes(fakeNodes{statuses: []node.Status{{Name: "node-a", Up: true, Advert: nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "codex", Command: "codex"}}}}}})
	r.SetModels(fakeBook{
		"/codex":       {Current: "GPT 5", Available: []string{"GPT 5", "GPT 5 mini"}},
		"node-a/codex": {Current: "GPT 5 mini"},
	})
	byID := map[string]Candidate{}
	for _, c := range r.All(t.Context()) {
		byID[c.Agent.ID] = c
	}
	if c := byID["codex"]; c.Model != "GPT 5" || len(c.Models) != 2 {
		t.Fatalf("hub codex = %+v", c)
	}
	if c := byID["pinned"]; c.Model != "gpt-5-mini" || len(c.Models) != 1 {
		t.Fatalf("pinned = %+v", c)
	}
	if c := byID["builder"]; c.Model != "GPT 5 mini" {
		t.Fatalf("builder = %+v", c)
	}
}

func mustCatalog(t *testing.T, agents map[string]agent.Config) *agent.Catalog {
	t.Helper()
	catalog, err := agent.NewCatalog(agents)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// Requirements are matched against what each machine reports: a tool it
// observed, hardware it has, a network it was declared on. A refusal names
// the atom and the reason; a kind the machine did not cover is unknown,
// not absent.
func TestRequirementsMatchTheSnapshot(t *testing.T) {
	catalog := mustCatalog(t, map[string]agent.Config{
		"local":   {Harness: "codex", Default: true},
		"builder": {Harness: "codex", Node: "node-a"},
		"shipper": {Harness: "codex", Node: "node-b"},
	})
	now := time.Now()
	seen := func(ok bool) []ability.Evidence {
		return []ability.Evidence{{Kind: ability.Observed, Method: "path", OK: ok, At: now}}
	}
	said := []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}
	snapA := &ability.Snapshot{Schema: ability.Schema, Node: "node-a", Coverage: map[ability.Kind]ability.Coverage{ability.Harness: ability.Complete, ability.Tool: ability.Complete, ability.Hardware: ability.Complete},
		Offers: []ability.Capability{
			{Kind: ability.Harness, ID: "codex", Evidence: seen(true)},
			{Kind: ability.Tool, ID: "docker", Evidence: seen(true)},
			{Kind: ability.Hardware, ID: "gpu", Evidence: seen(true)},
		}}
	snapB := &ability.Snapshot{Schema: ability.Schema, Node: "node-b", Coverage: map[ability.Kind]ability.Coverage{ability.Harness: ability.Complete, ability.Tool: ability.Complete, ability.Network: ability.Complete},
		Offers: []ability.Capability{
			{Kind: ability.Harness, ID: "codex", Evidence: seen(true)},
			{Kind: ability.Tool, ID: "docker", Evidence: seen(false), Detail: `"docker" not on this node's PATH`},
			{Kind: ability.Network, ID: "internal", Evidence: said},
			{Kind: ability.Tag, ID: "internal-net", Evidence: said},
		}}
	for _, sn := range []*ability.Snapshot{snapA, snapB} {
		if err := ability.Validate(sn); err != nil {
			t.Fatal(err)
		}
	}
	r := New(catalog)
	r.SetNodes(fakeNodes{statuses: []node.Status{
		{Name: "node-a", Up: true, Advert: nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "codex", Command: "codex"}}, Snapshot: snapA}},
		{Name: "node-b", Up: true, Advert: nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "codex", Command: "codex"}}, Snapshot: snapB}},
	}})
	r.SetHubAdvert(func() nodewire.Advert {
		return nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "codex", Command: "codex"}}, Capabilities: []string{"basic"}}
	})
	ids := func(cs []Candidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Agent.ID)
		}
		return out
	}
	if got := ids(r.Candidates(t.Context(), []string{"tool:docker", "hardware:gpu"}, nil)); len(got) != 1 || got[0] != "builder" {
		t.Fatalf("docker+gpu → %v", got)
	}
	if got := ids(r.Candidates(t.Context(), []string{"internal-net"}, nil)); len(got) != 1 || got[0] != "shipper" {
		t.Fatalf("tag internal-net → %v (a declared tag still counts, the legacy way)", got)
	}
	if got := ids(r.Candidates(t.Context(), []string{"network:internal"}, nil)); len(got) != 0 {
		t.Fatalf("a declared network placed work: %v", got)
	}
	if got := ids(r.Candidates(t.Context(), []string{"basic"}, nil)); len(got) != 1 || got[0] != "local" {
		t.Fatalf("bare tag → %v", got)
	}
	why := r.Explain(t.Context(), []string{"tool:docker"})
	if !strings.Contains(why, "shipper: lacks tool:docker (UNAVAILABLE") || !strings.Contains(why, "local: lacks tool:docker (UNKNOWN_COVERAGE") {
		t.Fatalf("explain = %s", why)
	}
}

// admittingNodes is a node source that can be asked for admission and
// remembers what it was asked.
type admittingNodes struct {
	fakeNodes
	refuse bool
	asked  []nodewire.AdmitRequest
}

func (a *admittingNodes) Release(context.Context, string, string) error { return nil }

func (a *admittingNodes) Bindings(context.Context, string, string) []ability.Binding { return nil }

func (a *admittingNodes) Admit(_ context.Context, name string, req nodewire.AdmitRequest) (ability.Admission, error) {
	a.asked = append(a.asked, req)
	adm := ability.Admission{Node: name, Source: ability.SourceNode, Verdict: ability.True, Code: ability.CodeAdmitted, Generation: 9, Sequence: 2, At: time.Now()}
	if a.refuse {
		adm.Verdict, adm.Code = ability.False, ability.CodeAbsent
		adm.Atoms = []ability.AtomResult{{Atom: ability.Text(req.Requirement), Verdict: ability.False, Code: ability.CodeAbsent}}
	}
	return adm, nil
}

// Admission asks the node only about the clauses it owns; what the hub
// owns — models — is judged here, and a hub-side refusal never reaches
// the node. A node's refusal is the verdict.
func TestAdmissionAsksTheNodeForItsOwnClauses(t *testing.T) {
	r := testRoster(t, nil, nil)
	nodes := &admittingNodes{fakeNodes: fakeNodes{statuses: []node.Status{
		up("node-a", []string{"gpu"}, nodewire.Harness{ID: "mock", Models: []string{"gpt-5"}}),
	}}}
	r.SetNodes(nodes)
	var builder Candidate
	for _, c := range r.All(t.Context()) {
		if c.Agent.ID == "builder" {
			builder = c
		}
	}
	if builder.Agent.ID == "" {
		t.Fatal("builder not in the roster")
	}
	adm, _, err := r.Admit(t.Context(), builder, []string{"tool:docker", "model:gpt-5"}, nil, "att-1")
	if err != nil {
		t.Fatal(err)
	}
	if !adm.OK() || adm.Source != ability.SourceNode || adm.Generation != 9 {
		t.Fatalf("admission = %+v", adm)
	}
	if len(nodes.asked) != 1 || ability.Text(nodes.asked[0].Requirement) != "tool:docker" || nodes.asked[0].Harness != "mock" || nodes.asked[0].Attempt != "att-1" {
		t.Fatalf("node was asked %+v, want only tool:docker for mock", nodes.asked)
	}
	// The hub refuses what it owns without asking the node.
	adm, _, err = r.Admit(t.Context(), builder, []string{"tool:docker", "model:claude*"}, nil, "att-2")
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Refused() || adm.Source != ability.SourceHub || len(nodes.asked) != 1 {
		t.Fatalf("a hub-owned refusal = %+v, node asked %d times", adm, len(nodes.asked))
	}
	// A node's refusal is the verdict.
	nodes.refuse = true
	adm, _, err = r.Admit(t.Context(), builder, []string{"tool:docker"}, nil, "att-3")
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Refused() || adm.Source != ability.SourceNode || adm.Unmet() == "" {
		t.Fatalf("node refusal = %+v", adm)
	}
	// A source that cannot be asked yields a cached verdict, never an error.
	r.SetNodes(fakeNodes{statuses: nodes.statuses})
	adm, _, err = r.Admit(t.Context(), builder, []string{"gpu"}, nil, "att-4")
	if err != nil || adm.Source != ability.SourceCached || !adm.OK() {
		t.Fatalf("cached admission = %+v, %v", adm, err)
	}
}

// A machine with no room for a worktree is not placed on, whatever its
// snapshot says it can do.
func TestAFullDiskBlocksPlacement(t *testing.T) {
	full := up("node-a", []string{"gpu"}, nodewire.Harness{ID: "mock"})
	full.Advert.Health = &nodewire.Health{DiskFree: 100 << 20, DiskTotal: 500 << 30}
	r := testRoster(t, []node.Status{full}, nil)
	for _, c := range r.All(t.Context()) {
		if c.Agent.ID == "builder" && (c.Eligible || !strings.Contains(c.Why, "disk nearly full")) {
			t.Fatalf("builder = eligible %v, why %q", c.Eligible, c.Why)
		}
	}
	roomy := up("node-a", []string{"gpu"}, nodewire.Harness{ID: "mock"})
	roomy.Advert.Health = &nodewire.Health{DiskFree: 50 << 30, DiskTotal: 500 << 30}
	r.SetNodes(fakeNodes{statuses: []node.Status{roomy}})
	for _, c := range r.All(t.Context()) {
		if c.Agent.ID == "builder" && !c.Eligible {
			t.Fatalf("builder blocked with room to spare: %q", c.Why)
		}
	}
}
