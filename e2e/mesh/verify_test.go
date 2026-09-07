package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
)

// C3a: a command verification runs ON THE NODE where the step ran. The
// check fails the first time and leaves a marker in the node's workspace;
// the retry lands on the same machine, sees the marker, and passes. The
// marker's existence over ssh is what proves where the check ran.
func TestC3VerifyCommandRunsOnTheNodeAndGatesDone(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())
	host := addrHost(addrB())
	_, _ = sshOut(t, host, "rm -f ~/steve-work/.verified")
	t.Cleanup(func() { _, _ = sshOut(t, host, "rm -f ~/steve-work/.verified") })

	created, err := f.plans.Create(plan.Plan{ProjectID: "local",
		TaskID: verificationTask(t, f, "verify"), Goal: "prove the check runs where the work is", By: "declared",
		Steps: []plan.Step{{
			ID: "work", Goal: "say worked", Requires: []string{"internal-net"}, State: plan.StepPending,
			// Fails once, plants the marker, passes next time.
			Verify: &plan.Verify{Kind: plan.VerifyCommand,
				Command: "test -e ~/steve-work/.verified || { touch ~/steve-work/.verified; echo 'first check: not yet' >&2; exit 1; }"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only node-b can do internal-net work; make sure the retry cannot
	// escape to a different machine, so the second check must be local
	// to the first's marker.
	sup := exec.NewSupervisor(planner.Rule{}, exec.Deps{
		Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts,
		Roster:   f.roster,
		Runner:   exec.NewAgentRunner(f.manager, noCaps{}, f.roster),
		Verifier: exec.NewVerifiers(f.registry, sharedAgentExecutor(t, f)),
	}, workflow.NewMemoryStore())
	sup.SetPlans(f.plans)
	sup.SetLedger(f.book, "mesh")
	sup.SetTasks(f.tasks)
	sup.SetExecution(f.executions)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	outcome, err := sup.Execute(ctx, created)
	if err != nil {
		t.Fatalf("the step never passed verification: %v", err)
	}
	if outcome.Recoveries != 1 {
		t.Fatalf("recoveries = %d; the check should have failed exactly once", outcome.Recoveries)
	}
	// The marker is on node-b, not on the hub: the check ran over there.
	out, err := sshOut(t, host, "ls ~/steve-work/.verified && echo MARKER-ON-NODE")
	if err != nil || !strings.Contains(out, "MARKER-ON-NODE") {
		t.Fatalf("the verification marker is not on %s: %v\n%s", nodeB, err, out)
	}
	final, _ := f.plans.Latest(created.ID)
	step, _ := final.Step("work")
	if step.State != plan.StepDone || step.Result == nil || !step.Result.Verified {
		t.Fatalf("step = %+v; done must mean verified", step)
	}
	t.Logf("verified on %s after %d failed check; marker present", nodeB, outcome.Recoveries)
}

// C3b: a second agent, on another machine, is asked to check the first
// one's work — and a FAIL verdict keeps the step out of done.
func TestC3AgentVerificationIsCrossMachineAndBinding(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())

	deps := exec.Deps{
		Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts,
		Roster:   f.roster,
		Runner:   exec.NewAgentRunner(f.manager, noCaps{}, f.roster),
		Verifier: exec.NewVerifiers(f.registry, sharedAgentExecutor(t, f)),
	}

	// Work on node-a, checked by the agent on node-b: passes.
	good, err := f.plans.Create(plan.Plan{ProjectID: "local", TaskID: verificationTask(t, f, "passing verification"), Goal: "cross-check", By: "declared",
		Steps: []plan.Step{{
			ID: "work", Goal: "say built", Requires: []string{"gpu"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	sup := exec.NewSupervisor(planner.Rule{}, deps, workflow.NewMemoryStore())
	sup.SetPlans(f.plans)
	sup.SetLedger(f.book, "mesh")
	sup.SetTasks(f.tasks)
	sup.SetExecution(f.executions)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := sup.Execute(ctx, good); err != nil {
		t.Fatalf("a PASS verdict from %s did not let the step through: %v", nodeB, err)
	}

	// The same shape, but the goal asks the verifier to reject: stays out
	// of done, and the failure names the verifier's reason.
	bad, err := f.plans.Create(plan.Plan{ProjectID: "local", TaskID: verificationTask(t, f, "failing verification"), Goal: "cross-check", By: "declared",
		Steps: []plan.Step{{
			ID: "work", Goal: "say built, reject me", Requires: []string{"gpu"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sup.Execute(ctx, bad)
	if err == nil {
		t.Fatal("a FAIL verdict was ignored")
	}
	if !strings.Contains(err.Error(), "verifier shipper") || !strings.Contains(err.Error(), "not where it was claimed") {
		t.Fatalf("err = %v, want the verifier's reason", err)
	}
	final, _ := f.plans.Latest(bad.ID)
	step, _ := final.Step("work")
	if step.State == plan.StepDone {
		t.Fatal("a step the verifier rejected reached done")
	}
	t.Logf("rejected as expected: %v", err)
}

// C7: a step's FINDING stops the run and produces a revision — planned by
// the agent on node-a — that keeps the finished step and adds what the
// finding demanded.
func TestC7FindingRevisesThePlanAndKeepsFinishedWork(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.registry.EnsureConnected(t.Context())
	brain, _ := f.catalog.Resolve("builder")

	created, err := f.plans.Create(plan.Plan{ProjectID: "local",
		TaskID: verificationTask(t, f, "revise after finding"), Goal: "ship, but the world changes underneath", By: "declared",
		Steps: []plan.Step{
			{ID: "build", Goal: "say built, surprise", Requires: []string{"gpu"}, State: plan.StepPending,
				Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"}},
			{ID: "stage", Goal: "say staged", Requires: []string{"internal-net"}, Needs: []string{"build"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"}},
			{ID: "ship", Goal: "say shipped", Requires: []string{"internal-net"}, Needs: []string{"stage"},
				State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sup := exec.NewSupervisor(
		planner.LLM{Agent: brain.ID, Executor: sharedAgentExecutor(t, f)},
		exec.Deps{Workspaces: f.artifacts, Attempts: f.attempts, Artifacts: f.artifacts, Roster: f.roster, Runner: exec.NewAgentRunner(f.manager, noCaps{}, f.roster)},
		workflow.NewMemoryStore(),
	)
	sup.SetPlans(f.plans)
	sup.SetLedger(f.book, "mesh")
	sup.SetTasks(f.tasks)
	sup.SetExecution(f.executions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := sup.Execute(ctx, created); err != nil {
		t.Fatalf("the plan did not survive its own finding: %v", err)
	}
	revisions := f.plans.Revisions(created.ID)
	if len(revisions) != 2 {
		t.Fatalf("revisions = %d, want the original and one revision", len(revisions))
	}
	last := revisions[1]
	if last.By != "llm:builder" || !strings.Contains(last.Because, "recheck") {
		t.Fatalf("revision = by %s because %q", last.By, last.Because)
	}
	if _, ok := last.Step("recheck"); !ok {
		t.Fatal("the revision did not add the step the finding demanded")
	}
	build, _ := last.Step("build")
	if build.State != plan.StepDone || build.Attempts != 1 {
		t.Fatalf("the finished step was not kept: %+v", build)
	}
	for _, id := range []string{"stage", "recheck", "ship"} {
		s, _ := last.Step(id)
		if s.State != plan.StepDone {
			t.Errorf("step %s ended %s", id, s.State)
		}
	}
	t.Logf("rev 2 by %s: %s", last.By, last.Because)
}
