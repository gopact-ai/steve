package turn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"

	"github.com/gopact-ai/steve/internal/attempt"
)

func TestRejectedRunningTransitionNeverPromptsTheAgent(t *testing.T) {
	runner := &fakeRunner{reply: "must not run"}
	c, tasks, book := taskCoordinatorBook(t, runner)
	// A trigger fails only the durable Running transition, after session setup.
	if _, err := book.DB().Exec(`CREATE TRIGGER deny_running BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='running' BEGIN SELECT RAISE(FAIL,'cannot record running'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := handle(c, t.Context(), "do work")
	if err == nil || !strings.Contains(err.Error(), "arm prompt execution") {
		t.Fatalf("execution arm failure=%v", err)
	}
	if got := runner.seen(); len(got) != 0 {
		t.Fatalf("unrecorded prompt ran: %v", got)
	}
	list := tasks.List("")
	if len(list) != 1 {
		t.Fatalf("tasks=%+v", list)
	}
	records, err := c.attempts.ForTask(t.Context(), list[0].ID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed {
		t.Fatalf("failed preparation not closed: %+v %v", records, err)
	}
}

type deadlineResponseRuntime struct {
	*fakeManager
	runner *deadlineResponseRunner
}

func (m deadlineResponseRuntime) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return m.runner, nil
}

type deadlineResponseRunner struct{ *fakeRunner }

func (r *deadlineResponseRunner) Prompt(ctx context.Context, input string, _ func(view.Progress)) (string, []string, error) {
	if strings.Contains(input, "neighbor") {
		return "neighbor still available", nil, nil
	}
	<-ctx.Done()
	return "", nil, &acp.Error{Code: acp.ErrorCodeInternalError, Message: "explicit response at deadline"}
}

func TestSettledErrorAtDeadlineKeepsAdjacentSessionAvailable(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	stateStore, err := state.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := &deadlineResponseRunner{fakeRunner: &fakeRunner{id: "session"}}
	manager := deadlineResponseRuntime{fakeManager: &fakeManager{}, runner: runner}
	c := newCoordinator(t, catalog, stateStore, capability.NewAssembler(nil), manager, 2*time.Second)
	if _, err := handle(c, t.Context(), "deadline response"); err == nil {
		t.Fatal("expected explicit agent error")
	}
	if runner.aborts.Load() != 0 || runner.cancels.Load() != 0 {
		t.Fatal("settled deadline response tore down shared host")
	}
	result, err := c.Handle(t.Context(), Request{ConversationID: "neighbor", Input: "neighbor"})
	if err != nil || result.Text != "neighbor still available" {
		t.Fatalf("neighbor session lost: %+v %v", result, err)
	}
}
