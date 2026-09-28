package turn

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

// toolCallContext serves one real steve_recall call for conversation's
// agent under scope and returns the context the call was served in,
// kept past the call the way work queued from it keeps it.
func toolCallContext(t *testing.T, grants agentmcp.Store, conversation string, scope agentmcp.GrantScope) context.Context {
	t.Helper()
	gate, err := agentmcp.New(0, i18n.New(i18n.LocaleZH))
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.SetStore(grants, nil); err != nil {
		t.Fatal(err)
	}
	calls := &recallContext{}
	gate.SetMemorizer(calls)
	gate.Extras(conversation, "codex", "schedule-token", "")
	if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: conversation, AgentID: "codex"}, scope); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gate.Start(serving) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if text, bad := scheduleToolCall(t, gate, "steve_recall", map[string]any{"query": "anything"}); bad {
		t.Fatalf("the tool call was refused: %s", text)
	}
	calls.mu.Lock()
	defer calls.mu.Unlock()
	queued := context.WithoutCancel(calls.ctx)
	if _, ok := agentmcp.ScopeFromContext(queued); !ok {
		t.Fatal("the tool call carried no grant")
	}
	return queued
}

// parentToolCallContext is the context a child's result reaches its
// parent's conversation in: the one the parent agent's steve_delegate
// call was served in, long after that call ended.
func parentToolCallContext(t *testing.T) context.Context {
	t.Helper()
	grants := &scheduleGrantStore{data: map[string]json.RawMessage{}}
	return toolCallContext(t, grants, "console:parent", agentmcp.GrantScope{TaskID: "parent-task", TaskEpoch: 1, AttemptID: "parent-turn", ExecutionGeneration: 1, NodeID: "hub", SessionID: "ns_parent"})
}

// contextsSeen keeps every context a fake dependency was driven under.
type contextsSeen struct {
	mu   sync.Mutex
	list []context.Context
}

func (s *contextsSeen) see(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, ctx)
}
func (s *contextsSeen) all() []context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]context.Context(nil), s.list...)
}

type observedRetainedManager struct {
	retainedTestManager
	seen *contextsSeen
}

func (m observedRetainedManager) AttachRetainedSession(ctx context.Context, place harness.Placement, id, workdir string) (harness.ResumableRunner, error) {
	m.seen.see(ctx)
	return m.retainedTestManager.AttachRetainedSession(ctx, place, id, workdir)
}

// observedNodes finds no other node, noting the context it was asked in.
type observedNodes struct{ seen *contextsSeen }

func (n observedNodes) Statuses() []node.Status { return nil }
func (n observedNodes) EnsureConnected(ctx context.Context, _ ...string) {
	n.seen.see(ctx)
}
func (n observedNodes) Admit(ctx context.Context, _ string, _ nodewire.AdmitRequest) (ability.Admission, error) {
	n.seen.see(ctx)
	return ability.Admission{}, nil
}
func (n observedNodes) Bindings(context.Context, string, string) []ability.Binding { return nil }
func (n observedNodes) Release(context.Context, string, string) error              { return nil }

// recoveryEntryRuns drives each recovery entry a console exchange can
// reach once its observer fails, under queued, and returns what the
// dependencies it drove were given.
func recoveryEntryRuns(t *testing.T, queued func(*testing.T, *Coordinator) context.Context) map[string][]context.Context {
	t.Helper()
	runs := map[string][]context.Context{}

	t.Run("resume retained chat", func(t *testing.T) {
		seen := &contextsSeen{}
		c, _, _, r, req := retainedChatFixture(t, withDeps(func(d *Deps) {
			d.Runtime = observedRetainedManager{retainedTestManager: d.Runtime.(retainedTestManager), seen: seen}
		}))
		ctx, cancel := context.WithTimeout(queued(t, c), waitDeadline)
		defer cancel()
		if _, err := c.ResumeRetainedChat(ctx, r.ID, req); err != nil {
			t.Fatal(err)
		}
		runs[t.Name()] = seen.all()
	})

	t.Run("resume retained plan", func(t *testing.T) {
		c, sup, identity, req := retainedPlanFixture(t)
		ctx, cancel := context.WithTimeout(queued(t, c), waitDeadline)
		defer cancel()
		if _, err := c.ResumeRetainedPlan(ctx, identity, req); err != nil {
			t.Fatal(err)
		}
		runs[t.Name()] = sup.seen.all()
	})

	t.Run("relocate chat", func(t *testing.T) {
		seen := &contextsSeen{}
		c, runner, _, old, req := retainedChatFixture(t, withDeps(func(d *Deps) {
			d.Fleet = roster.New(d.Catalog)
			d.Fleet.SetNodes(observedNodes{seen: seen})
		}))
		runner.state.State, runner.state.ProcessStopped = "interrupted", true
		runner.state.Command.ProcessStopped = true
		old, err := c.attempts.Get(t.Context(), old.ID)
		if err != nil {
			t.Fatal(err)
		}
		req.Relocation = &RelocationContext{Input: "original task"}
		plan, err := c.attempts.RecordRelocation(t.Context(), attempt.RelocationIntent{SourceID: old.ID, SourceRevision: old.Revision, TaskEpoch: old.Execution.Epoch, Checkpoint: "snapshot", Owner: req.SenderOpenID, CreatedAt: time.Now(), InputDigest: relocationRequestDigest(req), Prompt: "continue original", Target: attempt.Spec{ID: "replacement", TaskID: old.TaskID, TurnID: old.TurnID, Kind: old.Kind, Project: old.Project, Node: "node-b", Harness: old.Harness, Agent: old.Agent, Execution: old.Execution, ExecutionGeneration: attempt.SessionExecutionEpoch(old) + 1, NativeCommandID: "new-command", Scope: attempt.ScopePathSet, Base: "snapshot", Workspace: project.Workspace{ID: "replacement", Project: old.Project, Node: "node-b", Path: "/isolated", Kind: project.KindWorktree}}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(queued(t, c), waitDeadline)
		defer cancel()
		// No other node exists here; the relocation stops at target discovery.
		_, _ = c.RelocateChat(ctx, plan.ID, "confirm-stopped-and-retry:"+plan.ID, req)
		runs[t.Name()] = seen.all()
	})
	return runs
}

// A console exchange queued from a parent agent's steve_delegate call (a
// child's result delivered back) is recovered through these entries when
// its observer fails. Like Handle, they run on the hub's own authority:
// nothing they drive is handed the grant of the tool call behind them.
func TestRecoveryEntriesDoNotRunUnderTheGrantOfTheToolCallThatQueuedThem(t *testing.T) {
	runs := recoveryEntryRuns(t, func(t *testing.T, _ *Coordinator) context.Context { return parentToolCallContext(t) })
	if len(runs) != 3 {
		t.Fatalf("entries run: %d", len(runs))
	}
	for entry, seen := range runs {
		if len(seen) == 0 {
			t.Errorf("%s: drove nothing that could be observed", entry)
		}
		for _, ctx := range seen {
			if scope, ok := agentmcp.ScopeFromContext(ctx); ok {
				t.Errorf("%s: drove its work under the grant of attempt %s's tool call", entry, scope.AttemptID)
				break
			}
		}
	}
}
