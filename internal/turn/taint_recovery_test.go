package turn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// An interrupted turn leaves the conversation's session marked uncertain. As
// long as something may still be writing to it the next message waits, but
// once the interrupted attempt is over the conversation carries on by itself
// instead of asking the owner to start a new session by hand.
func TestUncertainSessionRecoversWithoutANewSessionCommand(t *testing.T) {
	c, runner, _, old, req := retainedChatFixture(t)
	req.Input = "继续"
	taint := c.text.T(i18n.Tainted, protocol.CommandNew)
	if _, err := c.Handle(t.Context(), req); err == nil || err.Error() != taint {
		t.Fatalf("live attempt did not hold the session: %v", err)
	}
	if !c.store.Conversation(req.ConversationID).Sessions["worker"].Tainted {
		t.Fatal("taint cleared while the original attempt was still live")
	}
	if _, err := c.attempts.ConfirmStopped(t.Context(), old.ID, "test", "node confirmed the original command ended"); err != nil {
		t.Fatal(err)
	}
	if left, err := c.attempts.Live(t.Context()); err != nil || len(left) != 0 {
		t.Fatalf("settled attempt still counts as live: %+v %v", left, err)
	}
	if _, err := c.Handle(t.Context(), req); err != nil && err.Error() == taint {
		t.Fatal("ended attempt still sent the owner to /new")
	}
	if c.store.Conversation(req.ConversationID).Sessions["worker"].Tainted {
		t.Fatal("taint survived an attempt that is over")
	}
	if saved := c.store.Conversation(req.ConversationID).Sessions["worker"]; saved.UpstreamID != runner.fakeRunner.id {
		t.Fatalf("recovered session lost its native context: %q", saved.UpstreamID)
	}
	record, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil || record.State != attempt.Failed {
		t.Fatalf("original attempt changed: %+v %v", record, err)
	}
}

// A turn that dies before it ever sends a prompt leaves the conversation's
// session exactly as it found it: nothing was in flight, so the next message
// runs instead of dead-ending on "start a new session first".
func TestTurnThatNeverPromptedLeavesNoUncertainSession(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	runner := &fakeRunner{id: "ns_live", reply: "ok"}
	manager := nativeManager{&fakeManager{runners: map[string]*fakeRunner{"codex": runner}, mcpHTTP: true}}
	c := New(catalog, sessions, capability.NewAssembler(nil), manager, time.Minute)
	c.SetProjects(projects, "p", "")
	c.SetTasks(tasks, "hub")
	c.SetAttempts(attempt.New(book))
	c.SetExecution(execution.New(t.Context(), tasks))
	c.SetAgentGate(&refusingGate{})
	if _, err := handle(c, t.Context(), "work"); err == nil {
		t.Fatal("refused grant did not fail the turn")
	}
	if prompts := runner.seen(); len(prompts) != 0 {
		t.Fatalf("turn prompted the agent anyway: %v", prompts)
	}
	if saved := sessions.Conversation("chat").Sessions["codex"]; saved.Tainted {
		t.Fatalf("undriven turn left the session uncertain: %+v", saved)
	}
}

// nativeManager hands out node-owned session ids, the only kind a grant is
// bound to.
type nativeManager struct{ *fakeManager }

func (m nativeManager) OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	session, err := m.fakeManager.OpenSession(ctx, at, upstreamID, workdir, servers)
	if err != nil {
		return nil, err
	}
	m.fakeManager.runners[at.Harness].id = "ns_live"
	return session, nil
}

// refusingGate injects the messaging capability and then refuses to bind the
// execution, the shape of a grant that cannot be handed to this turn.
type refusingGate struct{ fakeGate }

func (*refusingGate) BindExecution(context.Context, agentmcp.Binding, agentmcp.GrantScope) error {
	return errors.New("agent MCP execution grant is not authorized")
}
