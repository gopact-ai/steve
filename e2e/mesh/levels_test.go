package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

// C8: data levels gate placement across the fleet. node-a is cleared for
// public data only, node-b for restricted; a restricted project's step
// that needs the gpu (node-a) has nowhere to run and says why, while one
// that needs internal-net goes to node-b. A sealed project homed on the
// hub never leaves it.
func TestC8LevelsGatePlacementAndSealedStaysHome(t *testing.T) {
	requireMesh(t)
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{
		nodeA: {Addr: machines.Addr(nodeA), Token: machines.Token(nodeA), DialTimeout: 10 * time.Second, Level: "public"},
		nodeB: {Addr: machines.Addr(nodeB), Token: machines.Token(nodeB), DialTimeout: 10 * time.Second, Level: "restricted"},
	})
	t.Cleanup(reg.Close)
	reg.SetHubLevel("sealed")
	reg.EnsureConnected(t.Context())

	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":   {Harness: "mock", Default: true},
		"builder": {Harness: "mock", Node: nodeA, Requires: []string{"gpu"}},
		"shipper": {Harness: "mock", Node: nodeB, Requires: []string{"internal-net"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newFleet(t)
	f.manager.SetTransports(reg)
	fleetRoster := roster.New(catalog)
	fleetRoster.SetNodes(reg)
	fleetRoster.SetHubCapabilities([]string{"basic"})
	fleetRoster.SetHubLevel(project.LevelSealed)
	fleetRoster.SetNodeLevels(map[string]project.Level{nodeA: project.LevelPublic, nodeB: project.LevelRestricted})

	dir := t.TempDir()
	projects, attempts, artifacts := declareProjects(t, dir, reg,
		project.Project{ID: "secret", Level: project.LevelRestricted, Home: project.Home{Path: t.TempDir()}},
		project.Project{ID: "vault", Level: project.LevelSealed, Home: project.Home{Path: t.TempDir()}},
	)
	_ = projects
	deps := exec.Deps{Workspaces: artifacts, Attempts: attempts, Artifacts: artifacts, Roster: fleetRoster,
		Runner: exec.NewAgentRunner(f.manager, noCaps{}, fleetRoster), Recorder: f.plans}
	runs := exec.NewRuns(workflow.NewMemoryStore())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	step := func(id string, requires ...string) plan.Step {
		return plan.Step{ID: id, Goal: "say " + id, Requires: requires, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"}}
	}
	// Restricted: the gpu step cannot go to public node-a.
	created, err := f.plans.Create(plan.Plan{ProjectID: "secret", TaskID: "e2e-lvl-1", Goal: "levels", By: "declared",
		Steps: []plan.Step{step("gpu-work", "gpu")}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, _ := runs.Execute(ctx, created, deps)
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "public") {
		t.Fatalf("restricted work was placed on the public node: %v", outcome.Err)
	}
	// Restricted: the internal-net step runs on node-b, cleared for it.
	created, err = f.plans.Create(plan.Plan{ProjectID: "secret", TaskID: "e2e-lvl-2", Goal: "levels", By: "declared",
		Steps: []plan.Step{step("net-work", "internal-net")}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = runs.Execute(ctx, created, deps)
	if err != nil || outcome.Err != nil {
		t.Fatalf("restricted work on the restricted node failed: %v %v", err, outcome.Err)
	}
	final, _ := f.plans.Latest(created.ID)
	if final.Steps[0].Result == nil || final.Steps[0].Result.Node != nodeB {
		t.Fatalf("step ran on %+v, want node-b", final.Steps[0].Result)
	}
	// Sealed on the hub: a step needing a node is refused as sealed; one
	// the hub can do runs here.
	created, err = f.plans.Create(plan.Plan{ProjectID: "vault", TaskID: "e2e-lvl-3", Goal: "sealed", By: "declared",
		Steps: []plan.Step{step("leave", "internal-net")}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, _ = runs.Execute(ctx, created, deps)
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "sealed") {
		t.Fatalf("sealed work left its home: %v", outcome.Err)
	}
	created, err = f.plans.Create(plan.Plan{ProjectID: "vault", TaskID: "e2e-lvl-4", Goal: "sealed", By: "declared",
		Steps: []plan.Step{step("stay", "basic")}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = runs.Execute(ctx, created, deps)
	if err != nil || outcome.Err != nil {
		t.Fatalf("sealed work at home failed: %v %v", err, outcome.Err)
	}
	final, _ = f.plans.Latest(created.ID)
	if final.Steps[0].Result == nil || final.Steps[0].Result.Node != "" {
		t.Fatalf("sealed step ran on %+v, want the hub", final.Steps[0].Result)
	}
}
