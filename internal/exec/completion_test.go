package exec

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/view"
)

func TestStepOutcomesRecordObservedUsage(t *testing.T) {
	for _, failure := range []string{"", "prompt", "verify"} {
		t.Run(failure, func(t *testing.T) {
			art, att := stores(t)
			p := plan.Plan{ID: "plan", ProjectID: "p", TaskID: "task"}
			s := step("work", "do work", []string{"basic"})
			var workspace string
			want := &plan.Usage{Model: "model", Input: 100, Output: 20, CachedRead: 30, CachedWrite: 40, Context: 500, Reported: true}
			deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(_ context.Context, req StepRequest) (plan.StepResult, error) {
				workspace = req.Workspace
				if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("done"), 0600); err != nil {
					return plan.StepResult{}, err
				}
				out := plan.StepResult{Answer: "done", Usage: want}
				if failure == "prompt" {
					return out, errors.New("prompt failed after spending")
				}
				return out, nil
			})}
			if failure == "verify" {
				s.Verify = &plan.Verify{Kind: plan.VerifyCommand, Command: "test"}
				deps.Verifier = verifyFunc(func(StepRequest) error { return errors.New("verification failed") })
			}
			_, err := runStep(t.Context(), p, s, nil, deps)
			if (err != nil) != (failure != "") {
				t.Fatalf("run error=%v", err)
			}
			records, err := att.Closed(t.Context())
			if err != nil || len(records) != 1 || records[0].Usage == nil || *records[0].Usage != *attemptUsage(want) {
				t.Fatalf("closed=%+v err=%v", records, err)
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("settled workspace remains: %v", err)
			}
			name, found, err := art.Resolve(t.Context(), stepRef(p.TaskID, s.ID))
			if err != nil || found != (failure == "") || (found && name.Artifact != records[0].Result.Artifact) {
				t.Fatalf("name=%+v found=%v err=%v", name, found, err)
			}
		})
	}
}

func TestStepBudgetFailureReleasesAttemptAndWorkspace(t *testing.T) {
	art, att := stores(t)
	deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Budget: budgetFunc(func(string) (int, time.Time, error) { return 0, time.Time{}, errors.New("no remaining budget") })}
	_, err := runStep(t.Context(), plan.Plan{ID: "plan", ProjectID: "p", TaskID: "task"}, step("work", "work", []string{"basic"}), nil, deps)
	var noBudget ErrNoBudget
	if !errors.As(err, &noBudget) {
		t.Fatalf("run=%v", err)
	}
	live, _ := att.Live(t.Context())
	closed, _ := att.Closed(t.Context())
	if len(live) != 0 || len(closed) != 1 || closed[0].State != attempt.Failed {
		t.Fatalf("live=%+v closed=%+v", live, closed)
	}
	if _, err := os.Stat(closed[0].Workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("workspace survived budget failure: %v", err)
	}
}

func TestStepCannotBindAfterItsLeaseWasRevoked(t *testing.T) {
	art, att := stores(t)
	var workspace string
	s := step("work", "work", []string{"basic"})
	s.Verify = &plan.Verify{Kind: plan.VerifyCommand, Command: "verify"}
	deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(_ context.Context, req StepRequest) (plan.StepResult, error) {
		workspace = req.Workspace
		return plan.StepResult{Answer: "done", Usage: &plan.Usage{Input: 10, Reported: true}}, nil
	}), Verifier: verifyFunc(func(StepRequest) error {
		_, err := att.ExpireAll(t.Context(), "lost owner")
		return err
	})}
	_, err := runStep(t.Context(), plan.Plan{ID: "plan", ProjectID: "p", TaskID: "task"}, s, nil, deps)
	if err == nil {
		t.Fatal("lost attempt succeeded")
	}
	if _, found, err := art.Resolve(t.Context(), stepRef("task", "work")); err != nil || found {
		t.Fatalf("lost attempt bound result: found=%v err=%v", found, err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("uncommitted result discarded: %v", err)
	}
}

func TestStepUsageDistinguishesExplicitZeroFromMissing(t *testing.T) {
	for _, reported := range []bool{false, true} {
		spent := stepSpend{last: view.Progress{Usage: view.Usage{Reported: reported, ContextTokens: 500}}}
		u := attemptUsage(spent.usage())
		if u == nil || u.Reported != reported || u.Context != 500 || u.Input != 0 || u.Output != 0 {
			t.Fatalf("usage=%+v reported=%v", u, reported)
		}
	}
}

func TestStepCompletionFailureRetainsWorkspaceAndDoesNotRetry(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		dir := t.TempDir()
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { book.Close() })
		db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		trigger := `CREATE TRIGGER refuse_completion BEFORE UPDATE OF state ON operations WHEN NEW.kind = 'attempt' AND NEW.state = 'bound' BEGIN SELECT RAISE(FAIL, 'completion rejected'); END`
		if conflict {
			trigger = `CREATE TRIGGER move_result AFTER UPDATE OF state ON operations WHEN NEW.kind = 'attempt' AND NEW.state = 'bind-ready' BEGIN INSERT INTO names(name, version, artifact, updated_at) VALUES ('steve/task/work', 1, 'other-result', '2026-01-01T00:00:00Z'); END`
		}
		if _, err := db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		projects := project.Open(book)
		if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
			t.Fatal(err)
		}
		art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
		att := attempt.New(book)
		var workspace string
		calls := 0
		deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(_ context.Context, req StepRequest) (plan.StepResult, error) {
			calls++
			workspace = req.Workspace
			return plan.StepResult{Answer: "done", Usage: &plan.Usage{Input: 100, Reported: true}}, nil
		})}
		_, err = runStepWithRecovery(t.Context(), plan.Plan{ID: "plan", ProjectID: "p", TaskID: "task"}, step("work", "work", []string{"basic"}), nil, deps)
		if !errors.Is(err, ErrCompletion) || calls != 1 {
			t.Fatalf("commit failure reran work: calls=%d err=%v", calls, err)
		}
		name, found, err := art.Resolve(t.Context(), stepRef("task", "work"))
		if err != nil || found != conflict || (found && name.Artifact != "other-result") {
			t.Fatalf("failed transaction changed name: name=%+v found=%v err=%v", name, found, err)
		}
		if _, err := os.Stat(workspace); err != nil {
			t.Fatalf("failed completion discarded workspace: %v", err)
		}
		records, err := att.ForTask(t.Context(), "task")
		if err != nil || len(records) != 1 {
			t.Fatalf("records=%+v err=%v", records, err)
		}
		if conflict {
			if records[0].State != attempt.BindConflict || records[0].Result == nil || records[0].Usage == nil || records[0].Usage.Input != 100 {
				t.Fatalf("conflict lost candidate result or spend: %+v", records[0])
			}
		} else if records[0].State != attempt.Failed || records[0].Result == nil || records[0].Usage == nil || records[0].Usage.Input != 100 {
			t.Fatalf("failed completion did not close with candidate/spend: %+v", records[0])
		}
		if live, err := att.Live(t.Context()); err != nil || len(live) != 0 {
			t.Fatalf("failed completion left live leases: %+v %v", live, err)
		}
		for _, lease := range records[0].Leases {
			if err := book.Check(t.Context(), lease); !errors.Is(err, ledger.ErrStale) {
				t.Fatalf("failed completion retained lease: %+v %v", lease, err)
			}
		}
	}
}

type usageSessions struct{ closed bool }

func (s *usageSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return usageRunner{}, nil
}
func (s *usageSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.closed = true
	return nil
}

type usageRunner struct{}

func (usageRunner) ID() string { return "usage-test" }
func (usageRunner) Prompt(_ context.Context, _ string, progress func(view.Progress)) (string, []string, error) {
	progress(view.Progress{Settings: view.Settings{Model: "model"}, Usage: view.Usage{Reported: true, InputTokens: 100, ContextTokens: 500}})
	return "partial answer", nil, errors.New("prompt failed")
}
func (usageRunner) Cancel(context.Context) error { return nil }
func (usageRunner) Abort()                       {}

func TestAgentRunnerKeepsSpendWhenPromptFails(t *testing.T) {
	sessions := &usageSessions{}
	runner := NewAgentRunner(sessions, nil, testRoster(t, bothNodes()))
	result, err := runner.RunStep(t.Context(), StepRequest{Agent: "builder", Workspace: t.TempDir(), Goal: "work"})
	if err == nil || result.Answer != "partial answer" || result.Usage == nil || !result.Usage.Reported || result.Usage.Input != 100 || result.Usage.Context != 500 || !sessions.closed {
		t.Fatalf("error lost usage or cleanup: result=%+v closed=%v err=%v", result, sessions.closed, err)
	}
}
