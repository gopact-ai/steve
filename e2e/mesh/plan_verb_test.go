package mesh

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
)

// C-chat: the whole thing from the surface a person actually uses. Someone
// types a goal; Steve decomposes it, places each step on a machine that can
// run it, executes across three hosts, and reports the tree.
func TestChatPlanVerbAcrossTheFleet(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())

	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(
		f.catalog, store, capability.NewAssembler(nil), f.manager, 2*time.Minute,
	)
	coordinator.SetTasks(f.tasks, "hub-e2e")
	coordinator.SetProjects(f.projects, "local", "")
	coordinator.SetAttempts(f.attempts)
	coordinator.SetArtifacts(f.artifacts)

	// A declared workflow, so the test asserts placement rather than a
	// planner's taste: two branches on two different machines, then a merge.
	rules := planner.Rule{Workflows: map[string][]plan.Step{
		"release": {
			{
				ID: "build", Goal: "say built", Requires: []string{"gpu"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"},
			},
			{
				ID: "stage", Goal: "say staged", Requires: []string{"internal-net"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"},
			},
			{
				ID: "ship", Goal: "say shipped", Requires: []string{"internal-net"},
				Merge: []string{"build", "stage"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"},
			},
		},
	}}
	supervisor := exec.NewSupervisor(rules, exec.Deps{
		Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts,
		Roster: f.roster,
		Runner: exec.NewAgentRunner(f.manager, noCaps{}, f.roster),
	}, workflow.NewMemoryStore())
	supervisor.SetPlans(f.plans)
	supervisor.SetLedger(f.book, "mesh")
	supervisor.SetTasks(f.tasks)
	supervisor.Runs().Observe(f.view)
	coordinator.SetSupervisor(supervisor, f.plans, f.roster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// /fleet first: what a person checks before trusting a placement.
	fleetCard, err := coordinator.Handle(ctx, turn.Request{
		ConversationID: "chat", Input: "/fleet",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/fleet ->\n%s", fleetCard.Text)
	for _, want := range []string{nodeA, nodeB, "gpu", "internal-net"} {
		if !strings.Contains(fleetCard.Text, want) {
			t.Errorf("/fleet card is missing %q", want)
		}
	}

	result, err := coordinator.Handle(ctx, turn.Request{
		ConversationID: "chat", MessageID: "om_e2e", ChatID: "oc_e2e",
		Input: "/plan release the thing",
	})
	if err != nil {
		t.Fatalf("/plan failed: %v", err)
	}
	t.Logf("/plan ->\n%s", result.Text)

	if !strings.Contains(result.Text, "✓ **build**") {
		t.Error("build step is not reported as done")
	}
	// The card must say where each step ran: that is the whole reason the
	// fleet exists, and a plan that hides it cannot be checked.
	if !strings.Contains(result.Text, nodeA) {
		t.Errorf("the card does not say which step ran on %s", nodeA)
	}
	if !strings.Contains(result.Text, nodeB) {
		t.Errorf("the card does not say which step ran on %s", nodeB)
	}

	// /plans must then show it, with its revision history.
	listing, err := coordinator.Handle(ctx, turn.Request{ConversationID: "chat", Input: "/plans"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/plans ->\n%s", listing.Text)
	if !strings.Contains(listing.Text, "3/3") {
		t.Errorf("/plans does not show all three steps finished:\n%s", listing.Text)
	}
}
