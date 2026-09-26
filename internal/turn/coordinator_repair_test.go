package turn

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// fakeSupervisor records the plans it is asked to run and answers as told.
type fakeSupervisor struct {
	executed []plan.Plan
	fail     error
}

func (f *fakeSupervisor) Plan(context.Context, planner.Request) (plan.Plan, error) {
	return plan.Plan{}, errors.New("a repair is declared, never planned")
}
func (f *fakeSupervisor) Execute(_ context.Context, p plan.Plan) (exec.Outcome, error) {
	f.executed = append(f.executed, p)
	return exec.Outcome{}, f.fail
}
func (f *fakeSupervisor) Name() string                                       { return "fake" }
func (f *fakeSupervisor) PrepareRecovery(context.Context) error              { return nil }
func (f *fakeSupervisor) OpenRuns(context.Context) ([]exec.RunRecord, error) { return nil, nil }
func (f *fakeSupervisor) Resume(context.Context, exec.RunRecord) (exec.Outcome, error) {
	return exec.Outcome{}, nil
}

// flipNodes is a node source whose adverts change only when refreshed —
// the way a real node's do.
type flipNodes struct {
	mu       sync.Mutex
	statuses []node.Status
	fixed    bool
	refreshd []string
	// dials counts connection rounds a caller asked for.
	dials int
}

func (f *flipNodes) Statuses() []node.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]node.Status, len(f.statuses))
	copy(out, f.statuses)
	for i := range out {
		if !f.fixed {
			continue
		}
		hs := make([]nodewire.Harness, len(out[i].Advert.Harnesses))
		copy(hs, out[i].Advert.Harnesses)
		for j := range hs {
			hs[j].Missing = ""
		}
		out[i].Advert.Harnesses = hs
	}
	return out
}
func (f *flipNodes) EnsureConnected(context.Context, ...string) {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
}

func (f *flipNodes) dialed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}
func (f *flipNodes) Refresh(_ context.Context, name string) (nodewire.Advert, error) {
	f.mu.Lock()
	f.refreshd = append(f.refreshd, name)
	f.fixed = true
	f.mu.Unlock()
	for _, s := range f.Statuses() {
		if s.Name == name {
			return s.Advert, nil
		}
	}
	return nodewire.Advert{}, errors.New("no such node")
}

type recordCommands struct{ lines []string }

// repairMachines is the node registry a repair reads: adverts that change
// when refreshed, and file facts from each machine. No agent in these tests
// needs messaging or a held silence clock.
type repairMachines struct {
	*flipNodes
	*recordCommands
}

func (repairMachines) MCPEndpoint(_ context.Context, node string) (string, error) {
	return "", errors.New("no messaging endpoint on " + node)
}

func (repairMachines) RegisterIdle(string, idle.Clock) func() { return func() {} }

func (r *recordCommands) Files(_ context.Context, node string, req nodewire.FileRequest) (string, error) {
	r.lines = append(r.lines, node+": "+string(req.Op))
	return "/home/u/.local/bin:/usr/bin:/bin", nil
}

// recordProbes records the harnesses probed one at a time and answers a
// probe of every harness with results.
type recordProbes struct {
	probed  []string
	results []models.Result
}

func (r *recordProbes) Probe(_ context.Context, node, harness string) error {
	r.probed = append(r.probed, node+"/"+harness)
	return nil
}

func (r *recordProbes) ProbeAll(context.Context) []models.Result { return r.results }

func repairCoordinator(t *testing.T, opts ...testOption) (*Coordinator, *fakeSupervisor, *flipNodes, *recordCommands, *task.Store) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":   {Harness: "codex", Default: true},
		"kimi":    {Harness: "kimi", Node: "node-a"},
		"builder": {Harness: "codex", Node: "node-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.OpenLedger(testLedger(t))
	tasks, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	nodes := &flipNodes{statuses: []node.Status{{
		Name: "node-a", Up: true,
		Advert: nodewire.Advert{Node: "node-a", Harnesses: []nodewire.Harness{
			{ID: "kimi", Command: "kimi", Missing: `"kimi" not on this node's PATH`},
			{ID: "codex", Command: "codex"},
		}},
	}}}
	fleet := roster.New(catalog)
	fleet.SetNodes(nodes)
	sup := &fakeSupervisor{}
	cmds := &recordCommands{}
	opts = append([]testOption{
		withTasks(tasks, "laptop"),
		withDeps(func(d *Deps) { d.Fleet, d.Nodes = fleet, repairMachines{nodes, cmds} }),
		withCallbacks(func(cb *Callbacks) { cb.Supervisor = sup }),
	}, opts...)
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute, opts...)
	return c, sup, nodes, cmds, tasks
}

func say(t *testing.T, c *Coordinator, input string) Result {
	t.Helper()
	res, err := c.Handle(t.Context(), Request{
		ConversationID: "chat", ChatID: "chat", MessageID: "m-" + input, Input: input,
		SenderOpenID: "ou_owner", ChatType: protocol.ChatP2P, Mentioned: true,
	})
	if err != nil {
		t.Fatalf("%s: %v", input, err)
	}
	return res
}

// /repair turns a blocked agent into a one-step plan for a healthy agent on
// the same machine, verified by a command there, and re-checks the machine
// afterwards so the roster — not the helper — says whether it worked.
func TestRepairRunsAHelperAndReChecksTheMachine(t *testing.T) {
	probes := &recordProbes{}
	c, sup, nodes, cmds, tasks := repairCoordinator(t, withDeps(func(d *Deps) { d.Prober = probes }))

	// A healthy agent has nothing to repair.
	if res := say(t, c, "/repair builder"); !strings.Contains(res.Text, "没有坏") {
		t.Fatalf("healthy agent: %q", res.Text)
	}
	// The fleet says who could fix the broken one.
	if res := say(t, c, "/fleet"); !strings.Contains(res.Text, "/repair kimi") || !strings.Contains(res.Text, "builder") {
		t.Fatalf("fleet hint missing: %q", res.Text)
	}

	res := say(t, c, "/repair kimi")
	if len(sup.executed) != 1 {
		t.Fatalf("plans executed = %d", len(sup.executed))
	}
	p := sup.executed[0]
	if p.By != "repair" || !p.Fixed || len(p.Steps) != 1 {
		t.Fatalf("plan = %+v, want a fixed one-step repair", p)
	}
	// It runs under a project homed on the broken machine, not the chat's.
	if p.ProjectID != "builder" && p.ProjectID != "kimi" {
		t.Fatalf("project = %q, want one homed on node-a", p.ProjectID)
	}
	step := p.Steps[0]
	if step.Agent != "builder" {
		t.Fatalf("step pinned to %q, want the healthy agent on node-a", step.Agent)
	}
	if step.Verify == nil || step.Verify.Kind != plan.VerifyCommand || step.Verify.Command != "command -v kimi" {
		t.Fatalf("verify = %+v, want the binary's presence checked by command", step.Verify)
	}
	// The helper is told the machine, the binary and the PATH the node
	// process actually has, which came from a command on that machine.
	for _, want := range []string{"node-a", "`kimi`", "/home/u/.local/bin", "PATH"} {
		if !strings.Contains(step.Goal, want) {
			t.Fatalf("goal lacks %q: %s", want, step.Goal)
		}
	}
	if len(cmds.lines) != 1 || !strings.HasPrefix(cmds.lines[0], "node-a: ") {
		t.Fatalf("commands = %v", cmds.lines)
	}
	// Afterwards the node was asked to check itself, the new harness was
	// probed for its model, and the reply says it is ready now.
	if len(nodes.refreshd) != 1 || nodes.refreshd[0] != "node-a" {
		t.Fatalf("refreshed = %v", nodes.refreshd)
	}
	if len(probes.probed) != 1 || probes.probed[0] != "node-a/kimi" {
		t.Fatalf("probed = %v", probes.probed)
	}
	if !strings.Contains(res.Text, "修好了") || !strings.Contains(res.Text, "builder") {
		t.Fatalf("reply = %q", res.Text)
	}
	if res := say(t, c, "/repair kimi"); !strings.Contains(res.Text, "没有坏") {
		t.Fatalf("after repair: %q", res.Text)
	}
	// The repair ran as a task of its own, closed when done.
	var found bool
	for _, tk := range tasks.List("chat") {
		if strings.HasPrefix(tk.Goal, "repair kimi") {
			found = true
			if tk.State != task.StateDone {
				t.Fatalf("repair task state = %s", tk.State)
			}
		}
	}
	if !found {
		t.Fatal("no repair task on record")
	}
}

// Without the node registry a repair still runs, but it cannot ask the
// machine for its PATH or to check itself again: the helper is told the
// PATH is unknown, and the reply goes by the advert the machine last sent.
func TestRepairWithoutNodesKeepsTheLastAdvert(t *testing.T) {
	probes := &recordProbes{}
	c, sup, nodes, cmds, _ := repairCoordinator(t, withDeps(func(d *Deps) { d.Nodes, d.Prober = nil, probes }))
	res := say(t, c, "/repair kimi")
	if len(sup.executed) != 1 {
		t.Fatalf("plans executed = %d", len(sup.executed))
	}
	if goal := sup.executed[0].Steps[0].Goal; !strings.Contains(goal, "(unknown)") {
		t.Fatalf("goal does not say the PATH is unknown: %s", goal)
	}
	if len(cmds.lines) != 0 || len(nodes.refreshd) != 0 {
		t.Fatalf("commands = %v, refreshed = %v; want none without a registry", cmds.lines, nodes.refreshd)
	}
	if len(probes.probed) != 1 || probes.probed[0] != "node-a/kimi" {
		t.Fatalf("probed = %v", probes.probed)
	}
	if !strings.Contains(res.Text, "仍不可用") {
		t.Fatalf("reply = %q, want the harness still missing from the last advert", res.Text)
	}
}

// `/fleet probe` reports what probing every harness found, a failure as
// its error.
func TestFleetProbeReportsEveryHarness(t *testing.T) {
	probes := &recordProbes{results: []models.Result{
		{Endpoint: models.Endpoint{Node: "node-a", Harness: "kimi"}, Observation: models.Observation{Current: "k2"}},
		{Endpoint: models.Endpoint{Node: "node-a", Harness: "codex"}, Err: errors.New("handshake timed out")},
	}}
	c, _, _, _, _ := repairCoordinator(t, withDeps(func(d *Deps) { d.Prober = probes }))
	res := say(t, c, "/fleet probe")
	for _, want := range []string{"kimi** · k2", "✗ **node-a · codex", "handshake timed out"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("probe report lacks %q: %q", want, res.Text)
		}
	}
}

// A repair whose step fails, or whose machine still reports the harness
// missing afterwards, is reported as such — the helper's word is not the
// verdict.
func TestRepairReportsFailureHonestly(t *testing.T) {
	c, sup, nodes, _, _ := repairCoordinator(t)
	sup.fail = errors.New("verify: command -v kimi exited 1")
	if res := say(t, c, "/repair kimi"); !strings.Contains(res.Text, "没成功") || !strings.Contains(res.Text, "exited 1") {
		t.Fatalf("failed step: %q", res.Text)
	}
	if len(nodes.refreshd) != 0 {
		t.Fatal("a failed step must not pretend the machine changed")
	}
	// Nothing to do on a machine that is down.
	nodes.mu.Lock()
	nodes.statuses[0].Up = false
	nodes.statuses[0].LastError = "connection refused"
	nodes.mu.Unlock()
	if res := say(t, c, "/repair kimi"); !strings.Contains(res.Text, "修不了") {
		t.Fatalf("node down: %q", res.Text)
	}
}

// The context bar is computed by the rules a turn is judged by: an agent
// on another machine is usable, since the project is given a directory
// there, unless the project is sealed; a blocked agent carries the
// roster's reason, and the current agent is marked.
func TestContextSaysWhoCanWorkHere(t *testing.T) {
	c, _, _, _, _ := repairCoordinator(t)
	got, err := c.Context(t.Context(), "console:chat")
	if err != nil {
		t.Fatal(err)
	}
	// The default project is codex's own, homed on the hub.
	if got.Project == nil || got.Project.ID != "codex" || got.Project.Node != "laptop" && got.Project.Node != "hub" {
		t.Fatalf("project = %+v", got.Project)
	}
	byID := map[string]AgentChoice{}
	for _, a := range got.Agents {
		byID[a.ID] = a
	}
	if a := byID["codex"]; !a.Usable || !a.Ready || !a.Current {
		t.Fatalf("codex = %+v, want usable and current", a)
	}
	if a := byID["builder"]; !a.Usable || !a.Ready || !strings.Contains(a.Because, "codex") {
		t.Fatalf("builder = %+v, want usable, naming the project it attaches", a)
	}
	if a := byID["kimi"]; a.Usable || a.Ready || !strings.Contains(a.Because, "PATH") {
		t.Fatalf("kimi = %+v, want blocked with the roster's reason", a)
	}
	// Usable agents come first, and the verbs come with help.
	if !got.Agents[0].Usable {
		t.Fatalf("first agent %+v is not usable", got.Agents[0])
	}
	sealed, _, err := c.projects.Get(t.Context(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	sealed.Level = datalevel.Sealed
	if err := c.projects.Declare(t.Context(), []project.Project{sealed}); err != nil {
		t.Fatal(err)
	}
	if got, err = c.Context(t.Context(), "console:chat"); err != nil {
		t.Fatal(err)
	}
	for _, a := range got.Agents {
		if a.ID == "builder" && (a.Usable || !a.Ready || !strings.Contains(a.Because, "codex")) {
			t.Fatalf("builder = %+v, want ready but not usable for a sealed project, naming it", a)
		}
	}
	verbs := c.verbs()
	if len(verbs) < 15 || verbs[0].Command != "/plan" || verbs[0].Summary == "" {
		t.Fatalf("verbs = %+v", verbs[:2])
	}
}

// /fleet describes the fleet once, dialing what is down once, and judges
// every blocked agent's repair on that description. Completing /repair or
// /use while typing dials nothing: an offline node must not hold up a
// keystroke, once or once per blocked agent.
func TestFleetAndCompletionDescribeTheFleetOnce(t *testing.T) {
	c, _, nodes, _, _ := repairCoordinator(t)
	nodes.mu.Lock()
	nodes.statuses = append(nodes.statuses, node.Status{Name: "node-b", LastError: "connection refused"})
	nodes.mu.Unlock()
	for id, cfg := range map[string]agent.Config{
		"kimi-2": {Harness: "kimi", Node: "node-a"},
		"gemini": {Harness: "gemini", Node: "node-b"},
		"qwen":   {Harness: "qwen", Node: "node-b"},
	} {
		if err := c.catalog.Add(id, cfg); err != nil {
			t.Fatal(err)
		}
	}
	res := say(t, c, "/fleet")
	if n := nodes.dialed(); n != 1 {
		t.Fatalf("/fleet dialed %d times, want once", n)
	}
	for _, id := range []string{"kimi", "kimi-2"} {
		if !strings.Contains(res.Text, "/repair "+id) {
			t.Fatalf("/fleet lost the repair hint for %s:\n%s", id, res.Text)
		}
	}
	if strings.Contains(res.Text, "/repair gemini") {
		t.Fatalf("/fleet offered to repair an agent on a down node:\n%s", res.Text)
	}
	before := nodes.dialed()
	var labels []string
	for _, s := range c.Suggest(t.Context(), "chat", "/repair ") {
		labels = append(labels, s.Label)
	}
	if strings.Join(labels, ",") != "kimi,kimi-2" && strings.Join(labels, ",") != "kimi-2,kimi" {
		t.Fatalf("/repair completes %v, want the two agents builder can mend", labels)
	}
	if len(c.Suggest(t.Context(), "chat", "/use ")) != 6 {
		t.Fatal("/use should complete every agent")
	}
	if n := nodes.dialed() - before; n != 0 {
		t.Fatalf("completion dialed %d times", n)
	}
}

// Without a fleet there is nothing to find a helper in, and the refusal
// names the fleet, not the supervisor, as what is missing.
func TestRepairWithoutAFleetSaysTheFleetIsMissing(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	res := say(t, c, "/repair kimi")
	if strings.Contains(res.Text, "supervisor") || !strings.Contains(res.Text, "机群") {
		t.Fatalf("repair without a fleet: %q", res.Text)
	}
}
