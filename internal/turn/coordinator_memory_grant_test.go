package turn

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// endingGrantStore keeps grants the way scheduleGrantStore does, and
// authorizes a grant only until its execution ends.
type endingGrantStore struct {
	scheduleGrantStore
	ended atomic.Bool
}
type endingGrantTx struct {
	scheduleGrantTx
	ended *atomic.Bool
}

func (s *endingGrantStore) Update(ctx context.Context, fn func(agentmcp.StoreTx) error) error {
	return s.scheduleGrantStore.Update(ctx, func(tx agentmcp.StoreTx) error {
		return fn(endingGrantTx{tx.(scheduleGrantTx), &s.ended})
	})
}
func (t endingGrantTx) Authorize(agentmcp.Binding, agentmcp.GrantScope) error {
	if t.ended.Load() {
		return agentmcp.ErrGrantDenied
	}
	return nil
}

// recallContext answers steve_recall with nothing and keeps the context
// the call was served in.
type recallContext struct {
	mu  sync.Mutex
	ctx context.Context
}

func (r *recallContext) Remember(context.Context, string, string, string, string, string, string, string) (memory.Receipt, memory.Scope, error) {
	return memory.Receipt{}, memory.Scope{}, nil
}
func (r *recallContext) Recall(ctx context.Context, _, _, _, _ string, _ int) ([]memory.Hit, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx = ctx
	return nil, "", nil
}
func (r *recallContext) Forget(context.Context, string, string, string, string, string) (memory.Item, error) {
	return memory.Item{}, nil
}

// A child's result reaches its parent's conversation as a new owner turn,
// queued from work that began inside the parent agent's tool call. That
// turn runs on the hub's own authority: the grant of the tool call behind
// it, whose execution has ended by then, does not decide what the turn may
// read, so the project's memory still reaches it.
func TestAnOwnerTurnQueuedFromAnEndedToolCallStillGetsItsProjectMemory(t *testing.T) {
	book := testLedger(t)
	grants := &endingGrantStore{scheduleGrantStore: scheduleGrantStore{data: map[string]json.RawMessage{}}}
	shared := memory.NewLedgerStore(book)
	// The application guards the shared memory with the same check.
	shared.SetWriteGuard(func(ctx context.Context, _ *ledger.Tx) error {
		return grants.Update(ctx, func(tx agentmcp.StoreTx) error { return agentmcp.AuthorizeContext(ctx, tx) })
	})
	dir := t.TempDir()
	if err := home.Bootstrap(dir, memoryOwner); err != nil {
		t.Fatal(err)
	}
	c, _, runner := homeCoordinator(t, dir, memoryOwner, onLedger(book), withDeps(func(d *Deps) {
		d.Memory = memory.NewService(shared, filepath.Join(t.TempDir(), "audit.jsonl"))
	}))
	if err := c.projects.Declare(t.Context(), []project.Project{
		{ID: "home", Level: datalevel.Restricted, Home: project.Home{Path: dir}},
		{ID: "alpha", Home: project.Home{Path: t.TempDir()}},
	}); err != nil {
		t.Fatal(err)
	}
	c.homeProject = "home"
	const conversation = "console:chat"
	bind(t, c, conversation, "alpha")
	seedFact(t, c, memory.ProjectScope("alpha"), "deploys go through the staging cluster")

	// The parent agent's earlier turn, and the tool call it made in it.
	r := attempt.Record{Spec: attempt.Spec{ID: "parent-turn", Kind: attempt.KindChat, Project: "alpha", Node: "hub", Agent: "codex"}, State: attempt.Running, Session: "ns_parent"}
	if _, err := book.Begin(t.Context(), r.ID, "attempt", string(r.State), "", r); err != nil {
		t.Fatal(err)
	}
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
	if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: conversation, AgentID: "codex"}, agentmcp.GrantScope{TaskID: "parent-task", TaskEpoch: 1, AttemptID: r.ID, ExecutionGeneration: 1, NodeID: r.Node, SessionID: r.Session}); err != nil {
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
	if text, bad := scheduleToolCall(t, gate, "steve_recall", map[string]any{"query": "deploys"}); bad {
		t.Fatalf("the tool call was refused: %s", text)
	}
	calls.mu.Lock()
	queued := context.WithoutCancel(calls.ctx)
	calls.mu.Unlock()
	if _, ok := agentmcp.ScopeFromContext(queued); !ok {
		t.Fatal("the tool call carried no grant")
	}
	// The parent's turn is over by the time its child's result arrives.
	grants.ended.Store(true)

	ctx, cancel := context.WithTimeout(queued, waitDeadline)
	defer cancel()
	if _, err := c.Handle(ctx, Request{ConversationID: conversation, Input: "continue", SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P}); err != nil {
		t.Fatal(err)
	}
	seen := runner.seen()
	if len(seen) == 0 || !strings.Contains(seen[0], "deploys go through the staging cluster") {
		t.Fatalf("the turn did not get its project's memory: %q", seen)
	}
}
