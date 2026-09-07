package turn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
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
func (f *flipNodes) EnsureConnected(context.Context) {}
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

func (r *recordCommands) Files(_ context.Context, node string, req nodewire.FileRequest) (string, error) {
	r.lines = append(r.lines, node+": "+string(req.Op))
	return "/home/u/.local/bin:/usr/bin:/bin", nil
}

func repairCoordinator(t *testing.T) (*Coordinator, *fakeSupervisor, *flipNodes, *recordCommands, *task.Store) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":   {Harness: "codex", Default: true},
		"kimi":    {Harness: "kimi", Node: "node-a"},
		"builder": {Harness: "codex", Node: "node-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	c.SetTasks(tasks, "laptop")
	nodes := &flipNodes{statuses: []node.Status{{
		Name: "node-a", Up: true,
		Advert: nodewire.Advert{Node: "node-a", Harnesses: []nodewire.Harness{
			{ID: "kimi", Command: "kimi", Missing: `"kimi" not on this node's PATH`},
			{ID: "codex", Command: "codex"},
		}},
	}}}
	fleet := roster.New(catalog)
	fleet.SetNodes(nodes)
	plans, err := plan.Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	sup := &fakeSupervisor{}
	c.SetSupervisor(sup, plans, fleet)
	cmds := &recordCommands{}
	c.SetRepair(nodes, cmds)
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
	c, sup, nodes, cmds, tasks := repairCoordinator(t)
	var probed []string
	c.SetProber(func(_ context.Context, node, harness string) error {
		probed = append(probed, node+"/"+harness)
		return nil
	}, nil)

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
	if len(probed) != 1 || probed[0] != "node-a/kimi" {
		t.Fatalf("probed = %v", probed)
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
// on another machine is "not usable here" with the project's reason, a
// blocked agent carries the roster's, and the current agent is marked.
func TestContextSaysWhoCanWorkHere(t *testing.T) {
	c, _, _, _, _ := repairCoordinator(t)
	got, err := c.Context(t.Context(), "chat")
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
	if a := byID["builder"]; a.Usable || !a.Ready || !strings.Contains(a.Because, "codex") {
		t.Fatalf("builder = %+v, want ready but not usable here, naming the project", a)
	}
	if a := byID["kimi"]; a.Usable || a.Ready || !strings.Contains(a.Because, "PATH") {
		t.Fatalf("kimi = %+v, want blocked with the roster's reason", a)
	}
	// Usable agents come first, and the verbs come with help.
	if !got.Agents[0].Usable {
		t.Fatalf("first agent %+v is not usable", got.Agents[0])
	}
	verbs := c.Verbs()
	if len(verbs) < 15 || verbs[0].Command != "/plan" || verbs[0].Summary == "" {
		t.Fatalf("verbs = %+v", verbs[:2])
	}
}
