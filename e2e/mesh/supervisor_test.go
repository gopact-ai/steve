package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	osexec "os/exec"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/tui"
)

// fleet is the three-machine world under test: this hub plus the two nodes.
type fleet struct {
	catalog   *agent.Catalog
	registry  *node.Registry
	roster    *roster.Roster
	manager   *harness.Manager
	tasks     *task.Store
	plans     *plan.Store
	projects  *project.Store
	attempts  *attempt.Service
	artifacts *artifact.Store
	view      *readmodel.Model
}

// nodeWork is the directory every node's project is homed at.
const nodeWork = "/home/pengxiang.lpx/steve-work"

// declareProjects gives the fleet one project per place: "local" on the
// hub, and one named after each node, homed at that node's work directory.
func declareProjects(t *testing.T, dir string, nodes artifact.Nodes, extra ...project.Project) (*project.Store, *attempt.Service, *artifact.Store) {
	t.Helper()
	book, err := ledger.Open(filepath.Join(dir, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	ledgersMu.Lock()
	ledgers[dir] = book
	ledgersMu.Unlock()
	projects := project.Open(book)
	declared := []project.Project{
		{ID: "local", Home: project.Home{Path: t.TempDir()}},
		{ID: nodeA, Home: project.Home{Node: nodeA, Path: nodeWork}},
		{ID: nodeB, Home: project.Home{Node: nodeB, Path: nodeWork}},
	}
	declared = append(declared, extra...)
	if err := projects.Declare(t.Context(), declared); err != nil {
		t.Fatal(err)
	}
	return projects, attempt.New(book), artifact.New(filepath.Join(dir, "artifacts"), book, projects, nodes)
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	reg := registry(t)

	catalog, err := agent.NewCatalog(map[string]agent.Config{
		// A hub-local agent that deliberately lacks the node capabilities,
		// so placement has to be a real decision.
		"local": {Harness: "mock", Default: true},
		"builder": {
			Harness: "mock", Node: nodeA,
			Requires: []string{"gpu"},
		},
		"shipper": {
			Harness: "mock", Node: nodeB,
			Requires: []string{"internal-net"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The hub's own harness command is only used for hub-local placement;
	// each node starts the mock from its own config. So the hub needs a
	// mock it can actually run — built here, not the path on the nodes.
	manager, err := harness.NewManager(map[string]harness.Config{
		"mock": {Command: buildHubMock(t), Permission: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetTransports(reg)
	t.Cleanup(manager.Stop)

	fleetRoster := roster.New(catalog)
	fleetRoster.SetNodes(reg)
	fleetRoster.SetHubCapabilities([]string{"basic"})

	dir := t.TempDir()
	tasks, err := task.Open(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.Open(filepath.Join(dir, "plans.json"))
	if err != nil {
		t.Fatal(err)
	}

	projects, attempts, artifacts := declareProjects(t, dir, reg)
	view := readmodel.New(readmodel.Sources{
		Hub:    readmodel.Hub{Node: "hub-e2e", Started: time.Now(), Capabilities: []string{"basic"}},
		Roster: fleetRoster, Nodes: reg, Tasks: tasks, Plans: plans,
		Ledger: readmodel.Ledger{Attempts: attempts, Artifacts: artifacts, Projects: projects},
	})
	return &fleet{catalog: catalog, registry: reg, roster: fleetRoster, manager: manager, tasks: tasks, plans: plans,
		projects: projects, attempts: attempts, artifacts: artifacts, view: view}
}

// buildHubMock compiles the mock agent for the hub's own placements.
func buildHubMock(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := osexec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, out)
	}
	return bin
}

// noCaps assembles nothing: these steps need no identity or MCP servers, and
// this keeps the test about placement and execution.
type noCaps struct{}

func (noCaps) Assemble(roster.Candidate) (string, []acp.MCPServer, error) { return "", nil, nil }

// C1+C2: a plan fans out across two machines by capability and converges on
// a third step, with real agents on real hosts.
func TestC1PlacementAndFanOutAcrossHosts(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)

	created, err := f.plans.Create(plan.Plan{ProjectID: "local",
		TaskID: "e2e-1", Goal: "build on the gpu box, stage on the internal box, then converge",
		By: "declared",
		Steps: []plan.Step{
			{
				ID: "build", Goal: "say built", Requires: []string{"gpu"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e fixture"},
			},
			{
				ID: "stage", Goal: "say staged", Requires: []string{"internal-net"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e fixture"},
			},
			{
				ID: "ship", Goal: "say shipped", Requires: []string{"internal-net"},
				Merge: []string{"build", "stage"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e fixture"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	runner := exec.NewAgentRunner(f.manager, noCaps{}, f.roster)
	runs := exec.NewRuns(workflow.NewMemoryStore())
	runs.Observe(f.view)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	outcome, err := runs.Execute(ctx, created, exec.Deps{Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts, Roster: f.roster, Runner: runner, Recorder: f.plans})
	if err != nil {
		t.Fatalf("plan failed across the fleet: %v", err)
	}
	if len(outcome.Output.Results) == 0 {
		t.Fatal("the plan produced no results")
	}
	for _, r := range outcome.Output.Results {
		t.Logf("terminal step %s: agent=%s node=%s answer=%q",
			r.StepID, r.Result.Agent, r.Result.Node, r.Result.Answer)
		if r.Result.Node == "" {
			t.Errorf("step %s reports no node; placement was not recorded", r.StepID)
		}
	}

	// C2: the two branches really overlapped in time on two machines. The
	// recorded results carry their own clocks, so this is measured, not
	// assumed.
	final, _ := f.plans.Latest(created.ID)
	build, _ := final.Step("build")
	stage, _ := final.Step("stage")
	if build.Result == nil || stage.Result == nil {
		t.Fatal("branch results were not recorded")
	}
	overlap := build.Result.StartedAt.Before(stage.Result.EndedAt) && stage.Result.StartedAt.Before(build.Result.EndedAt)
	if !overlap {
		t.Errorf("branches ran serially: build %s–%s, stage %s–%s",
			build.Result.StartedAt.Format("15:04:05.000"), build.Result.EndedAt.Format("15:04:05.000"),
			stage.Result.StartedAt.Format("15:04:05.000"), stage.Result.EndedAt.Format("15:04:05.000"))
	}
	if build.Result.Node == stage.Result.Node {
		t.Errorf("both branches ran on %s; they were placed to spread", build.Result.Node)
	}
	t.Logf("fan-out overlapped: build on %s, stage on %s", build.Result.Node, stage.Result.Node)

	// The change stream must have carried the transitions to the renderers.
	var started, completed int
	for _, ev := range f.view.Recent() {
		switch {
		case strings.HasSuffix(ev.Kind, "started"):
			started++
		case strings.HasSuffix(ev.Kind, "completed"):
			completed++
		}
	}
	if started < 3 || completed < 3 {
		t.Errorf("stream saw %d starts and %d completions, want at least 3 of each", started, completed)
	}
	t.Logf("read model observed %d starts, %d completions", started, completed)
}

// C4: a node dying mid-plan moves its step to a machine that can still do it.
func TestC4NodeLossRePlacesTheStep(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	host := addrHost(addrB())
	t.Cleanup(func() { _, _ = sshOut(t, host, "~/steve-bin/nodectl start") })

	// Both agents can satisfy "work"; only one node will survive.
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"builder": {Harness: "mock", Node: nodeA, Default: true},
		"shipper": {Harness: "mock", Node: nodeB},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.roster = roster.New(catalog)
	f.roster.SetNodes(f.registry)

	created, err := f.plans.Create(plan.Plan{ProjectID: "local",
		TaskID: "e2e-2", Goal: "survive a node going away", By: "declared",
		Steps: []plan.Step{{
			ID: "work", Goal: "say done", Requires: []string{"prod-cred"},
			State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e fixture"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// node-b holds prod-cred; take it away and nothing can run the step.
	if out, err := sshOut(t, host, "~/steve-bin/nodectl stop"); err != nil {
		t.Fatalf("stop node-b: %v\n%s", err, out)
	}
	// Let the registry notice.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.registry.Advert(t.Context(), nodeB); err != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	runner := exec.NewAgentRunner(f.manager, noCaps{}, f.roster)
	runs := exec.NewRuns(workflow.NewMemoryStore())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	outcome, err := runs.Execute(ctx, created, exec.Deps{Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts, Roster: f.roster, Runner: runner})
	if err == nil {
		t.Fatal("the step ran even though the only capable node was down")
	}
	if !strings.Contains(err.Error(), "prod-cred") {
		t.Fatalf("failure = %v, want it to name the unsatisfiable requirement", err)
	}
	t.Logf("refused with: %v", err)
	if outcome.Recoveries != 0 {
		t.Errorf("retried %d times against a roster that cannot satisfy the step", outcome.Recoveries)
	}
}

// B1+B3+B4: the read model reports the whole fleet, and both renderers see it.
func TestB1ReadModelAndRenderers(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	if _, err := f.registry.Advert(t.Context(), nodeA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Advert(t.Context(), nodeB); err != nil {
		t.Fatal(err)
	}

	server, err := readmodel.NewServer(f.view, readmodel.ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	res, err := http.Get(server.URL() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var snap readmodel.Snapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}

	if len(snap.Nodes) != 3 || snap.Nodes[0].Role != readmodel.RoleHub {
		t.Fatalf("snapshot has %d nodes, want the hub and both workers", len(snap.Nodes))
	}
	byName := map[string]readmodel.Node{}
	for _, n := range snap.Nodes {
		byName[n.Name] = n
		t.Logf("node %s up=%v %s/%s caps=%v harnesses=%d",
			n.Name, n.Up, n.OS, n.Arch, n.Capabilities, len(n.Harnesses))
	}
	if !byName[nodeA].Up || !byName[nodeB].Up {
		t.Fatal("a live node was reported down")
	}
	if len(byName[nodeA].Harnesses) == 0 {
		t.Fatal("node-a reported no harnesses")
	}
	// The ledger's facts ride the same snapshot the dashboard and steve
	// top render: every list is there, empty or not, never missing.
	raw, _ := json.Marshal(snap.Facts)
	for _, key := range []string{"reservations", "attestations", "replicas", "disclosures", "effects", "grants"} {
		if !strings.Contains(string(raw), `"`+key+`":[`) {
			t.Fatalf("snapshot facts lack %q: %s", key, raw)
		}
	}
	if snap.Attempts == nil || snap.Landings == nil {
		t.Logf("attempts/landings nil in snapshot (%d/%d)", len(snap.Attempts), len(snap.Landings))
	}
	// The page can act: a /fleet sent through the console comes back from
	// the same coordinator the chat uses, naming the real nodes.
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(f.catalog, store, capability.NewAssembler(nil), f.manager, 2*time.Minute)
	coordinator.SetProjects(f.projects, "local", "")
	coordinator.SetAttempts(f.attempts)
	coordinator.SetArtifacts(f.artifacts)
	coordinator.SetIdentity("ou_owner", nil)
	consoleSup := exec.NewSupervisor(planner.Rule{}, exec.Deps{Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts, Roster: f.roster,
		Runner: exec.NewAgentRunner(f.manager, noCaps{}, f.roster), Recorder: f.plans}, nil)
	consoleSup.SetPlans(f.plans)
	coordinator.SetSupervisor(consoleSup, f.plans, f.roster)
	server.SetConsole(console.New(coordinator, "ou_owner", f.view))
	payload, _ := json.Marshal(map[string]string{"conversation": "console:main", "input": "/fleet"})
	req, _ := http.NewRequest(http.MethodPost, server.URL()+"/console/send", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var sent struct {
		Reply readmodel.Reply `json:"reply"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	if res2.StatusCode != http.StatusOK || !strings.Contains(sent.Reply.Text, nodeA) || !strings.Contains(sent.Reply.Text, nodeB) {
		t.Fatalf("console /fleet = %d %+v %s", res2.StatusCode, sent.Reply, sent.Error)
	}
	t.Logf("console /fleet ->\n%s", sent.Reply.Text)
	// The roster must reflect the real adverts, not the config's hopes.
	ready := 0
	for _, a := range snap.Agents {
		t.Logf("agent %-8s where=%-7s eligible=%v %s", a.ID, orHub(a.Node), a.Eligible, a.Why)
		if a.Eligible {
			ready++
		}
	}
	if ready < 2 {
		t.Fatalf("only %d agents eligible; the roster is not seeing the live nodes", ready)
	}

	// The console is served by the same process, from the same model: the
	// shell names its bundle, and the bundle subscribes to the change
	// stream and can act through the console endpoint.
	page, err := http.Get(server.URL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	shell, _ := io.ReadAll(page.Body)
	page.Body.Close()
	m := regexp.MustCompile(`assets/index-[A-Za-z0-9_-]+\.js`).Find(shell)
	if m == nil {
		t.Fatalf("the shell names no bundle: %s", shell)
	}
	asset, err := http.Get(server.URL() + "/" + string(m))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := io.ReadAll(asset.Body)
	asset.Body.Close()
	if asset.StatusCode != http.StatusOK || !strings.Contains(string(bundle), "/events") || !strings.Contains(string(bundle), "console/send") {
		t.Fatalf("the bundle (%d, %d bytes) does not subscribe to the stream or reach the console", asset.StatusCode, len(bundle))
	}
	t.Logf("console at %s (%s, %d bytes)", server.URL(), m, len(bundle))
}

func orHub(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

// B3: the TUI renders the same fleet the dashboard does, from the same
// endpoint. It is a client, not a second reader of the stores, so the two
// surfaces cannot disagree about what is happening.
func TestB3TUIRendersTheFleet(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())

	// Give it something to draw: a task tree and a plan.
	parent, err := f.tasks.Create(task.Task{
		Goal: "ship the release", Channel: "chat", Member: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Begin(parent.ID, "local", "hub-e2e", ""); err != nil {
		t.Fatal(err)
	}
	child, err := f.tasks.Create(task.Task{
		Goal: "build it on the gpu box", Channel: "chat", Member: "builder",
		Node: nodeA, Parent: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Begin(child.ID, "builder", nodeA, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.plans.Create(plan.Plan{ProjectID: "local",
		TaskID: parent.ID, Goal: "ship the release", By: "declared",
		Steps: []plan.Step{
			{ID: "build", Goal: "compile", Requires: []string{"gpu"}, State: plan.StepDone,
				Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
				Result: &plan.StepResult{Agent: "builder", Node: nodeA}},
			{ID: "ship", Goal: "release", Requires: []string{"internal-net"}, Needs: []string{"build"},
				State: plan.StepRunning, Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: "make test"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, err := readmodel.NewServer(f.view, readmodel.ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	screen := tui.New(tui.Config{URL: server.URL()})
	frame := screen.Once(t.Context())
	t.Logf("\n%s", frame)

	for _, want := range []string{nodeA, nodeB, "builder", "shipper", "NODES", "AGENTS", "TASKS"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame is missing %q", want)
		}
	}
	// The tree, not a flat list: the child must be indented under its
	// parent. Compare on the plain text — the frame is full of ANSI.
	plain := stripANSI(frame)
	if !strings.Contains(plain, "└ #"+child.ID) {
		t.Errorf("the delegated task is not drawn under its parent:\n%s", plain)
	}
	if !strings.Contains(plain, "○ snapshot") {
		t.Error("a one-shot frame should say it is a snapshot, not that the gateway is offline")
	}
	// Live capabilities, read from the nodes themselves.
	if !strings.Contains(frame, "gpu") || !strings.Contains(frame, "internal-net") {
		t.Error("node capabilities are missing from the frame")
	}
}

// stripANSI removes colour so assertions read the text, not the escapes.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func placementOf(node, harnessID string) harness.Placement {
	return harness.Placement{Node: node, Harness: harnessID}
}

var (
	ledgersMu sync.Mutex
	ledgers   = map[string]*ledger.Ledger{}
)

// ledgerOf is the ledger declareProjects opened under dir.
func ledgerOf(t *testing.T, dir string) *ledger.Ledger {
	t.Helper()
	ledgersMu.Lock()
	defer ledgersMu.Unlock()
	book, ok := ledgers[dir]
	if !ok {
		t.Fatalf("no ledger under %s", dir)
	}
	return book
}
