package mesh

import (
	"context"
	"encoding/json"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// realFleet is the three-machine world with real models in it: the hub's
// own claude and codex, and whichever harnesses the nodes advertise and can
// actually answer with. Every session here costs real tokens.
type realFleet struct {
	*fleet
	assembler *capability.Assembler
}

func newRealFleet(t *testing.T) *realFleet {
	t.Helper()
	reg := registry(t)

	// The hub's harnesses are the same commands config.json uses. The nodes
	// start theirs from their own node.json; only the harness ids matter to
	// the hub, which is the point — the command line is the node's fact.
	manager, err := harness.NewManager(map[string]harness.Config{
		"claude-code": {Command: "npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}, Permission: "auto"},
		"codex":       {Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}, Permission: "auto"},
		"kimi":        {Command: "kimi", Args: []string{"acp"}, Permission: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetTransports(reg)
	t.Cleanup(manager.Stop)

	hubWork := filepath.Join(t.TempDir(), "hub-work")
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		// The planning brain and the delegating caller both live on the hub.
		"claude": {Harness: "claude-code", Default: true},
		// Real workers on the nodes, placed by capability.
		"coder-a": {Harness: "codex", Node: nodeA, Requires: []string{"gpu"}},
		"coder-b": {Harness: "codex", Node: nodeB, Requires: []string{"internal-net"}},
		"kimi-b":  {Harness: "kimi", Node: nodeB, Requires: []string{"internal-net"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fleetRoster := roster.New(catalog)
	fleetRoster.SetNodes(reg)
	fleetRoster.SetHubCapabilities([]string{"basic"})

	dir := t.TempDir()
	tasks, err := task.Open(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasks.SetBudget(30, 2*time.Hour)
	plans, err := plan.Open(filepath.Join(dir, "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	view := readmodel.New(readmodel.Sources{
		Hub:    readmodel.Hub{Node: "hub-e2e", Started: time.Now(), Capabilities: []string{"basic"}},
		Roster: fleetRoster, Nodes: reg, Tasks: tasks, Plans: plans,
	})
	projects, attempts, artifacts := declareProjects(t, dir, reg,
		project.Project{ID: "real", Home: project.Home{Path: hubWork}},
		project.Project{ID: "real-a", Home: project.Home{Node: nodeA, Path: nodeWork + "/real-a"}},
		project.Project{ID: "real-b", Home: project.Home{Node: nodeB, Path: nodeWork + "/real-b"}},
	)
	return &realFleet{
		fleet: &fleet{catalog: catalog, registry: reg, roster: fleetRoster, manager: manager,
			tasks: tasks, plans: plans, view: view, projects: projects, attempts: attempts, artifacts: artifacts},
		assembler: capability.NewAssembler(nil),
	}
}

// realCaps gives a step's agent the reporting contract and nothing else: no
// identity files, so the test is about planning and placement, not persona.
type realCaps struct{}

func (realCaps) Assemble(roster.Candidate) (string, []acp.MCPServer, error) {
	return exec.ReportingContract, nil, nil
}

// Real-1: a person states a goal; the hub's real claude decomposes it; real
// codex on the nodes does the work; `go test` runs on the node that did it.
//
// The assertions are structural on purpose. A real model's plan is not a
// fixture, so the test does not dictate the steps — it checks that whatever
// was planned validated, was placed on the machines that could do it, ran,
// and was verified where it ran. The plan and the card are logged in full
// so a person can judge the planning quality themselves.
func TestRealClaudePlansRealCodexExecutesOnTheNodes(t *testing.T) {
	requireReal(t)
	f := newRealFleet(t)
	f.registry.EnsureConnected(t.Context())
	for _, host := range []string{addrHost(addrA()), addrHost(addrB())} {
		_, _ = sshOut(t, host, "rm -rf ~/steve-work/real-a ~/steve-work/real-b; mkdir -p ~/steve-work/real-a ~/steve-work/real-b")
	}

	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(f.catalog, store, f.assembler, f.manager, 15*time.Minute)
	coordinator.SetTasks(f.tasks, "hub-e2e")
	coordinator.SetProjects(f.projects, "local", "")
	coordinator.SetAttempts(f.attempts)
	coordinator.SetArtifacts(f.artifacts)

	brain, _ := f.catalog.Resolve("claude")
	supervisor := exec.NewSupervisor(
		planner.LLM{
			Agent: brain.ID, Sessions: f.manager, Workspaces: f.artifacts,
			At:      harness.Placement{Node: brain.Node, Harness: brain.Harness},
			Timeout: 6 * time.Minute,
		},
		exec.Deps{
			Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts,
			Roster:   f.roster,
			Runner:   exec.NewAgentRunner(f.manager, realCaps{}, f.roster),
			Verifier: exec.NewVerifiers(f.registry, f.manager, f.roster, f.artifacts),
		},
		workflow.NewMemoryStore(),
	)
	supervisor.SetPlans(f.plans)
	supervisor.Runs().Observe(f.view)
	coordinator.SetSupervisor(supervisor, f.plans, f.roster)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	started := time.Now()
	goal := "/plan 做一个最小的 Go 模块 calc：在有 gpu 的机器上实现 Add(a, b int) int 并写单元测试；" +
		"在有 internal-net 的机器上写一份 calc 的 README.md 说明 API 和用法。" +
		"每台机器各自在自己的工作目录里做，不需要互相拷贝文件。" +
		"Go 在 /usr/local/go/bin/go。验证用命令，不要用 none。"
	result, err := coordinator.Handle(ctx, turn.Request{
		ConversationID: "chat", MessageID: "om_real", ChatID: "oc_real", Input: goal,
	})
	elapsed := time.Since(started).Round(time.Second)
	if err != nil {
		t.Fatalf("/plan failed after %s: %v", elapsed, err)
	}
	t.Logf("/plan took %s ->\n%s", elapsed, result.Text)

	stored, ok := f.plans.ForTask(taskIDFrom(t, f.tasks))
	if !ok {
		t.Fatal("no plan was recorded")
	}
	planJSON, _ := json.MarshalIndent(stored, "", "  ")
	t.Logf("plan as stored:\n%s", planJSON)

	if stored.By != "llm:claude" {
		t.Errorf("plan by %q, want the real planning agent", stored.By)
	}
	if len(stored.Steps) < 2 {
		t.Fatalf("a two-machine goal became %d step(s)", len(stored.Steps))
	}
	nodesUsed := map[string]bool{}
	verifiedByCommand := 0
	for _, s := range stored.Steps {
		if s.Result == nil {
			t.Errorf("step %s has no result", s.ID)
			continue
		}
		if s.State != plan.StepDone {
			t.Errorf("step %s ended %s: %s", s.ID, s.State, s.Result.Error)
		}
		nodesUsed[s.Result.Node] = true
		if s.Verify != nil && s.Verify.Kind == plan.VerifyCommand && s.Result.Verified {
			verifiedByCommand++
		}
	}
	if !nodesUsed[nodeA] || !nodesUsed[nodeB] {
		t.Errorf("steps ran on %v, want both nodes", keys(nodesUsed))
	}
	if verifiedByCommand == 0 {
		t.Error("no step was verified by a command run on its node")
	}

	// The work landed in the project's canonical directory on the hub: the
	// steps ran in worktrees on the nodes, published their results, and the
	// merge step's result was merged in. The plan chooses the layout — a
	// module named calc may well live in calc/ — so look for the module
	// wherever it was put rather than dictating where.
	// The node's toolchain may be newer than the hub's; the module was
	// already verified where it was built, so the hub-side check runs on a
	// copy with the go directive pinned to the hub's own version.
	landed, _, _ := f.projects.Get(ctx, "local")
	out, err := osexec.Command("/bin/sh", "-c",
		"rm -rf /tmp/steve-landed-check && cp -r "+landed.Home.Path+" /tmp/steve-landed-check && cd /tmp/steve-landed-check && "+
			"find . -name go.mod | head -3 && cd \"$(dirname \"$(find . -name go.mod | head -1)\")\" && "+
			"sed -i -e \"s/^go .*/go $(/usr/local/go/bin/go env GOVERSION | sed 's/^go//' | cut -d. -f1,2)/\" -e '/^toolchain /d' go.mod && "+
			"env -u GOROOT GOTOOLCHAIN=local GOFLAGS=-mod=mod /usr/local/go/bin/go test ./... 2>&1 | tail -3").CombinedOutput()
	t.Logf("hub project module:\n%s", out)
	if err != nil || (!strings.Contains(string(out), "ok") && !strings.Contains(string(out), "PASS")) {
		t.Errorf("go test in the landed project did not pass: %v\n%s", err, out)
	}
	out, _ = osexec.Command("/bin/sh", "-c", "cd "+landed.Home.Path+" && find . -iname 'README*' | head -3").CombinedOutput()
	t.Logf("hub project docs:\n%s", out)
	if !strings.Contains(strings.ToUpper(string(out)), "README") {
		t.Errorf("no README landed in the hub project")
	}
	// The nodes kept nothing: worktrees are discarded after publish.
	if out, err := sshOut(t, addrHost(addrA()), "ls ~/steve-work/worktrees/ 2>/dev/null | wc -l"); err == nil && strings.TrimSpace(out) != "0" {
		t.Errorf("worktrees left behind on %s: %s", nodeA, strings.TrimSpace(out))
	}
}

// Real-2: a real model, mid-turn on the hub, is told it lacks a capability
// and has the steve_delegate tool. Does it use it, and does the work land
// on the machine that has the capability? This is the whole "agents
// collaborate" claim, with a real model deciding to collaborate.
func TestRealClaudeDelegatesToTheNodeThatCan(t *testing.T) {
	requireReal(t)
	f := newRealFleet(t)
	f.registry.EnsureConnected(t.Context())
	hostB := addrHost(addrB())
	_, _ = sshOut(t, hostB, "mkdir -p ~/steve-work/real-b && rm -f ~/steve-work/real-b/staged.txt")

	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	go func() { _ = gate.Start(ctx) }()

	service := delegate.New(f.tasks, f.roster, f.manager, f.assembler, f.artifacts, "hub-e2e")
	service.SetLedger(f.attempts, f.artifacts)
	service.SetGate(gate)
	service.SetEndpoints(f.registry)
	service.MaxSilence = 10 * time.Minute
	gate.SetDelegator(service)
	f.registry.SetMCPDialer(loopbackDialer(gate.Addr()))

	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(f.catalog, store, f.assembler, f.manager, 15*time.Minute)
	coordinator.SetTasks(f.tasks, "hub-e2e")
	coordinator.SetProjects(f.projects, "local", "")
	coordinator.SetAttempts(f.attempts)
	coordinator.SetArtifacts(f.artifacts)
	coordinator.SetAgentGate(gate)
	coordinator.SetNodeEndpoints(f.registry)

	started := time.Now()
	result, err := coordinator.Handle(ctx, turn.Request{
		ConversationID: "chat", MessageID: "om_deleg", ChatID: "oc_deleg",
		Input: "你在 hub 上，这台机器没有内网访问（internal-net）。" +
			"需要在有 internal-net 能力的机器上、它自己的工作目录里创建文件 staged.txt，内容只有一行 STAGED。" +
			"你自己做不到，请用 steve_delegate 工具把这件事交给能做的 agent（requires 填 [\"internal-net\"]），" +
			"等它返回后，把它的 task_id、agent、node 和 outcome 原样汇报给我，不要自己编。",
	})
	elapsed := time.Since(started).Round(time.Second)
	if err != nil {
		t.Fatalf("turn failed after %s: %v", elapsed, err)
	}
	t.Logf("turn took %s ->\n%s", elapsed, result.Text)

	// A real child task under the caller's task, done, on node-b.
	var child task.Task
	for _, candidate := range f.tasks.List("chat") {
		if candidate.Parent != "" {
			child = candidate
		}
	}
	if child.ID == "" {
		t.Fatal("the model did not delegate: no child task exists")
	}
	t.Logf("child task #%s: member=%s node=%s state=%s goal=%q", child.ID, child.Member, child.Node, child.State, child.Goal)
	if child.Node != nodeB || child.State != task.StateDone {
		t.Errorf("child = %s on %s, want done on %s", child.State, child.Node, nodeB)
	}
	parent, _ := f.tasks.Get(child.Parent)
	if parent.Budget.Turns < 2 {
		t.Errorf("parent budget %+v; the child's spend was not charged", parent.Budget)
	}

	// The child ran on node-b in its own worktree; its result was published
	// and landed in the parent's project once the parent's turn released
	// the canonical lock — so the file is in the project, on the hub, and
	// nothing was left behind on node-b.
	landed, _, _ := f.projects.Get(ctx, "local")
	raw, err := os.ReadFile(filepath.Join(landed.Home.Path, "staged.txt"))
	if err != nil || !strings.Contains(string(raw), "STAGED") {
		t.Errorf("staged.txt in the landed project: %v %q", err, raw)
	} else {
		t.Logf("staged.txt landed in %s: %q", landed.Home.Path, strings.TrimSpace(string(raw)))
	}
	if out, err := sshOut(t, hostB, "ls ~/steve-work/worktrees/ | wc -l"); err == nil && strings.TrimSpace(out) != "0" {
		t.Errorf("worktrees left behind on %s: %s", nodeB, strings.TrimSpace(out))
	}
	// And the model reported what came back rather than inventing it.
	if !strings.Contains(result.Text, child.ID) {
		t.Errorf("the report does not mention the child task id %s", child.ID)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// loopbackDialer reaches the hub's messaging server for the reverse tunnel,
// the way `steve run` wires it.
func loopbackDialer(addr string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}
