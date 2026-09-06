package turn

import (
	"context"
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRejectedRunningTransitionNeverPromptsTheAgent(t *testing.T) {
	runner := &fakeRunner{reply: "must not run"}
	c, _ := taskCoordinator(t, runner)
	// A trigger fails only the durable Running transition, after session setup.
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	c.SetAttempts(attempt.New(book))
	if _, err := book.DB().Exec(`CREATE TRIGGER deny_running BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='running' BEGIN SELECT RAISE(FAIL,'cannot record running'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = handle(c, t.Context(), "do work")
	if err == nil || !strings.Contains(err.Error(), "arm prompt execution") {
		t.Fatalf("execution arm failure=%v", err)
	}
	if got := runner.seen(); len(got) != 0 {
		t.Fatalf("unrecorded prompt ran: %v", got)
	}
	records, err := c.attempts.Closed(t.Context())
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
	stateStore, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
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
