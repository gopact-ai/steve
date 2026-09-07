package exec

import (
	"context"
	"errors"
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/view"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskPauseReachesPlanStepAndBlocksLateResult(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, _ := tasks.Create(task.Task{Channel: "c"})
	r := execution.New(t.Context(), tasks)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	deps := Deps{Executions: r, Workspaces: art, Artifacts: art, Attempts: att, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(ctx context.Context, req StepRequest) (plan.StepResult, error) {
		entered <- ctx
		<-release
		return plan.StepResult{Answer: "late", Usage: &plan.Usage{Input: 7, Reported: true}}, nil
	})}
	go func() {
		_, err := runStep(t.Context(), plan.Plan{ID: "plan", ProjectID: "p", TaskID: tracked.ID}, step("work", "work", []string{"basic"}), nil, deps)
		done <- err
	}()
	runnerCtx := <-entered
	ids, err := tasks.SetAside(tracked.ID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	if runnerCtx.Err() == nil {
		t.Fatal("pause missed plan runner")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late successful runner bypassed cancellation")
	}
	if err := waiting.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := art.Resolve(t.Context(), stepRef(tracked.ID, "work")); err != nil || found {
		t.Fatalf("paused step bound: %v %v", found, err)
	}
	records, _ := att.ForTask(t.Context(), tracked.ID)
	if len(records) != 1 || records[0].Usage == nil || records[0].Usage.Input != 7 {
		t.Fatalf("paused step lost spend: %+v", records)
	}
}

type slotSessions struct{ opened, closed int }

func (s *slotSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	s.opened++
	return slotSession{}, nil
}
func (s *slotSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.closed++
	return nil
}

type slotSession struct{}

func (slotSession) ID() string { return "slot-session" }
func (slotSession) Prompt(_ context.Context, prompt string, p func(view.Progress)) (string, []string, error) {
	p(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 1}})
	if strings.Contains(prompt, "PASS 或 FAIL") {
		return "PASS", nil, nil
	}
	return "done", nil, nil
}
func (slotSession) Cancel(context.Context) error { return nil }
func (slotSession) Abort()                       {}

func TestVerifierCanUseWorkersSingleEndpointSlotAfterSessionClosed(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, _ := tasks.Create(task.Task{Channel: "c"})
	registry := execution.New(t.Context(), tasks)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}, "checker": {Harness: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(catalog)
	fleet.SetHubSlots(map[string]int{"mock": 1})
	sessions := &slotSessions{}
	budget := budgetFunc(func(id string) (int, time.Time, error) {
		tracked, err := tasks.ReserveTurn(id)
		var deadline time.Time
		if tracked.Budget.MaxElapsed > 0 {
			deadline = tracked.Deadline(time.Now())
		}
		return tracked.Budget.MaxTurns - tracked.Budget.Turns, deadline, err
	})
	auxiliary := agentexec.New(sessions, fleet, art, att, registry, budget)
	deps := Deps{Executions: registry, Workspaces: art, Artifacts: art, Attempts: att, Roster: fleet, Runner: NewAgentRunner(sessions, nil, fleet), Verifier: NewVerifiers(nil, auxiliary)}
	work := step("work", "work", nil)
	work.Agent = "worker"
	work.Verify = &plan.Verify{Kind: plan.VerifyAgent, Agent: "checker"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := runStep(ctx, plan.Plan{ID: "plan", ProjectID: "p", TaskID: tracked.ID}, work, nil, deps)
	if err != nil || !result.Verified {
		t.Fatalf("single-slot verification deadlocked or failed: %+v %v", result, err)
	}
	if sessions.opened != 2 || sessions.closed != 2 {
		t.Fatalf("session lifecycle: %+v", sessions)
	}
	records, err := att.ForTask(t.Context(), tracked.ID)
	if err != nil || len(records) != 2 {
		t.Fatalf("attempts=%+v %v", records, err)
	}
	for _, record := range records {
		if record.State != attempt.Bound || record.Usage == nil || record.Usage.Input != 1 {
			t.Fatalf("attempt not settled: %+v", record)
		}
	}
}

func TestUnconfirmedRemoteVerifierKeepsStepWorkspace(t *testing.T) {
	art, att := stores(t)
	work := step("work", "work", []string{"gpu"})
	work.Verify = &plan.Verify{Kind: plan.VerifyCommand, Command: "check"}
	var workspace string
	deps := Deps{Workspaces: art, Artifacts: art, Attempts: att, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(ctx context.Context, req StepRequest) (plan.StepResult, error) {
		workspace = req.Workspace
		return plan.StepResult{Answer: "done"}, nil
	}), Verifier: NewVerifiers(unconfirmedCommand{}, nil)}
	_, err := runStep(t.Context(), plan.Plan{ID: "plan", ProjectID: "p", TaskID: "task"}, work, nil, deps)
	if !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("missing exit proof was lost: %v", err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatal("possibly active verifier workspace removed", err)
	}
	records, err := att.ForTask(t.Context(), "task")
	if err != nil || len(records) != 1 || !records[0].Unsettled {
		t.Fatalf("verifier writer not quarantined: %+v %v", records, err)
	}
}

type unconfirmedCommand struct{}

func (unconfirmedCommand) Exec(context.Context, string, string, string) (string, error) {
	return "", context.Canceled
}
