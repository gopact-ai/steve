package mesh

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// C-auto: nobody declared a workflow. The person states a goal; a planning
// agent on node-a decomposes it; the steps land on the machines that can do
// them; the card says who ran what where.
func TestAutoPlanDecomposesAndPlacesAcrossTheFleet(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())

	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(f.catalog, store, capability.NewAssembler(nil), f.manager, 3*time.Minute)
	coordinator.SetTasks(f.tasks, "hub-e2e")
	coordinator.SetProjects(f.projects, "local", "")
	coordinator.SetAttempts(f.attempts)
	coordinator.SetArtifacts(f.artifacts)

	// The planner is an agent on node-a: Steve does not call a model API,
	// it opens a session on one of its own agents and holds it to the
	// contract. The mock answers a brief with a real, validating plan.
	brain, _ := f.catalog.Resolve("builder")
	supervisor := exec.NewSupervisor(
		planner.LLM{
			Agent: brain.ID, Executor: sharedAgentExecutor(t, f),
		},
		exec.Deps{Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts, Roster: f.roster, Runner: exec.NewAgentRunner(f.manager, noCaps{}, f.roster)},
		workflow.NewMemoryStore(),
	)
	supervisor.SetPlans(f.plans)
	supervisor.SetLedger(f.book, "mesh")
	supervisor.SetTasks(f.tasks)
	supervisor.SetExecution(f.executions)
	coordinator.SetExecution(f.executions)
	supervisor.Runs().Observe(f.view)
	coordinator.SetSupervisor(supervisor, f.plans, f.roster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := coordinator.Handle(ctx, turn.Request{
		ConversationID: "chat", MessageID: "om_auto", ChatID: "oc_auto",
		Input: "/plan get the release out the door",
	})
	if err != nil {
		t.Fatalf("/plan failed: %v", err)
	}
	t.Logf("/plan ->\n%s", result.Text)

	if !strings.Contains(result.Text, "llm:builder") {
		t.Error("the plan was not attributed to the planning agent")
	}
	for _, want := range []string{"✓ **build**", "✓ **stage**", "✓ **ship**"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("card is missing %q", want)
		}
	}
	// Decomposed by a model, placed by capability: the two branches must
	// have landed on different machines.
	if !strings.Contains(result.Text, "@"+nodeA) || !strings.Contains(result.Text, "@"+nodeB) {
		t.Errorf("steps did not spread across both nodes:\n%s", result.Text)
	}
	stored, ok := f.plans.ForTask(taskIDFrom(t, f.tasks))
	if !ok {
		t.Fatal("no plan recorded for the task")
	}
	if stored.By != "llm:builder" || len(stored.Steps) != 3 {
		t.Fatalf("stored plan = by %s, %d steps", stored.By, len(stored.Steps))
	}
}

// C-delegate: an agent on the hub, mid-turn, hands a piece of work to
// whichever agent has the capability — on another machine — through the
// MCP tool, over the real loopback server, and gets a typed result back.
func TestAgentDelegatesAcrossMachinesThroughTheTool(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())
	f.tasks.SetBudget(12, time.Hour)

	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gate.Start(ctx) }()

	service := delegate.New(f.tasks, f.roster, f.manager, capability.NewAssembler(nil), f.artifacts, "hub-e2e")
	service.SetLedger(f.attempts, f.artifacts)
	service.SetGate(gate)
	service.SetEndpoints(f.registry)
	gate.SetDelegator(service)

	// The calling agent: a hub-local task, mid-turn, holding a token.
	parent, err := f.tasks.Create(task.Task{ProjectID: "local", Goal: "assemble the release", Channel: "chat", Member: "local", Node: "hub-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Begin(parent.ID, "local", "hub-e2e", ""); err != nil {
		t.Fatal(err)
	}
	extras := gate.Extras("chat", "local", "parent-token", "")
	if len(extras) != 1 {
		t.Fatal("no messaging capability for the parent")
	}

	// Call steve_delegate the way the agent's MCP client would.
	call := func(args map[string]any) (map[string]any, error) {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "steve_delegate", "arguments": args},
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gate.URL(), strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer parent-token")
		res, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad response: %s", raw)
		}
		return out, nil
	}

	// The tool is listed, so an agent can discover it.
	listBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "tools/list"})
	lreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, gate.URL(), strings.NewReader(string(listBody)))
	lreq.Header.Set("Content-Type", "application/json")
	lreq.Header.Set("Authorization", "Bearer parent-token")
	lres, err := http.DefaultClient.Do(lreq)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := io.ReadAll(lres.Body)
	lres.Body.Close()
	if !strings.Contains(string(listed), "steve_delegate") {
		t.Fatalf("steve_delegate is not offered:\n%s", listed)
	}

	// Delegate by capability: whoever has internal-net — that is node-b.
	out, err := call(map[string]any{
		"goal": "stage the build on the internal network", "requires": []string{"internal-net"},
		"refs": []string{"git release/1.0"}, "expect": "staged and reachable",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool result: %v", out["result"])
	resultText := textOf(t, out)
	var got agentmcp.DelegateResult
	if err := json.Unmarshal([]byte(resultText), &got); err != nil {
		t.Fatalf("result is not a typed DelegateResult: %s", resultText)
	}
	if got.Agent != "shipper" || got.Node != nodeB || got.Outcome != "ok" {
		t.Fatalf("delegated to %s@%s outcome=%s, want shipper@%s ok", got.Agent, got.Node, got.Outcome, nodeB)
	}
	// The child answered from node-b with the context it was given.
	for _, want := range []string{"stage the build", "assemble the release", "git: release/1.0", "internal-net"} {
		if !strings.Contains(got.Answer, want) {
			t.Errorf("child on %s did not see %q in its context", nodeB, want)
		}
	}

	// The tree: a child under the parent, done, funded from and charged to it.
	child, ok := f.tasks.Get(got.TaskID)
	if !ok || child.Parent != parent.ID || child.State != task.StateDone || child.Node != nodeB {
		t.Fatalf("child task = %+v", child)
	}
	charged, _ := f.tasks.Get(parent.ID)
	if charged.Budget.Turns < 2 {
		t.Fatalf("parent budget = %+v; the child's spend was not charged", charged.Budget)
	}

	// A cycle is refused by the structure: the child cannot hand work back
	// up to its own ancestor. Simulate the child calling from its task.
	childExtras := gate.Delegated("chat", "shipper", child.ID, "local", "child-token", "")
	_ = childExtras
	if _, err := f.tasks.Begin(child.ID, "shipper", nodeB, ""); err == nil {
		// The child is done; re-opening it just to test the cycle would
		// be dishonest. Use the structural check directly instead.
		t.Fatal("a done task was reopened")
	}
	if _, err := f.tasks.Spawn(child.ID, task.Task{Goal: "back to local", Member: "local"}); err == nil {
		t.Fatal("delegating back to an ancestor was accepted")
	} else {
		t.Logf("cycle refused: %v", err)
	}
}

func textOf(t *testing.T, rpc map[string]any) string {
	t.Helper()
	result, ok := rpc["result"].(map[string]any)
	if !ok {
		t.Fatalf("rpc error: %v", rpc["error"])
	}
	if result["isError"] == true {
		t.Fatalf("tool error: %v", result["content"])
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("empty tool result")
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func taskIDFrom(t *testing.T, tasks *task.Store) string {
	t.Helper()
	all := tasks.List("chat")
	if len(all) == 0 {
		t.Fatal("no task was opened for /plan")
	}
	return all[0].ID
}
