package turn

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

type relocationResourceProbe struct {
	entered                  chan struct{}
	release                  chan struct{}
	probes, admits, releases atomic.Int32
}

func (n *relocationResourceProbe) Statuses() []node.Status { return nil }
func (n *relocationResourceProbe) EnsureConnected(ctx context.Context, _ ...string) {
	if n.probes.Add(1) == 1 {
		close(n.entered)
	}
	select {
	case <-ctx.Done():
	case <-n.release:
	}
}
func (n *relocationResourceProbe) Admit(context.Context, string, nodewire.AdmitRequest) (ability.Admission, error) {
	n.admits.Add(1)
	return ability.Admission{}, nil
}
func (n *relocationResourceProbe) Bindings(context.Context, string, string) []ability.Binding {
	return nil
}
func (n *relocationResourceProbe) Release(context.Context, string, string) error {
	n.releases.Add(1)
	return nil
}

func TestDuplicateRelocationCannotReachOriginalDriversResources(t *testing.T) {
	c, runner, _, old, req := retainedChatFixture(t)
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
	nodes := &relocationResourceProbe{entered: make(chan struct{}), release: make(chan struct{})}
	c.fleet = roster.New(c.catalog)
	c.fleet.SetNodes(nodes)
	done := make(chan error, 1)
	go func() {
		_, err := c.RelocateChat(t.Context(), plan.ID, "confirm-stopped-and-retry:"+plan.ID, req)
		done <- err
	}()
	select {
	case <-nodes.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first driver did not enter target discovery")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = c.RelocateChat(ctx, plan.ID, "confirm-stopped-and-retry:"+plan.ID, req)
	if err == nil || !strings.Contains(err.Error(), "busy") || nodes.probes.Load() != 1 || nodes.admits.Load() != 0 || nodes.releases.Load() != 0 {
		t.Fatalf("duplicate driver crossed target resource boundary: err=%v probes=%d admits=%d releases=%d", err, nodes.probes.Load(), nodes.admits.Load(), nodes.releases.Load())
	}
	close(nodes.release)
	if err := <-done; err == nil {
		t.Fatal("fixture unexpectedly found another node")
	}
}
