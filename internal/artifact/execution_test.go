package artifact

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestStoppedChildCannotLandUnderParentLeaseOrFromPending(t *testing.T) {
	canonical := t.TempDir()
	write(t, canonical, "a", "before")
	s, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	tasks, err := task.OpenLedger(s.ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := tasks.Create(task.Task{Member: "parent", Channel: "c"})
	child, err := tasks.Spawn(parent.ID, task.Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(child.ID)
	s.SetExecution(execution.New(t.Context(), tasks))
	ws, err := s.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "att-child"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "a", "after")
	result, _, err := s.Publish(t.Context(), ws, ws.Base, "att-child", "result")
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Execution: &token, AttemptID: "att-child"}
	if err := s.Defer(t.Context(), p.ID, result.ID, "child", source); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ledger.Acquire(t.Context(), "canonical:"+p.ID, "parent", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.SetAside(child.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LandUnder(t.Context(), p, result.ID, "child", lease, source); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("stopped landing=%v", err)
	}
	if err := s.ledger.Check(t.Context(), lease); err != nil {
		t.Fatal("child stop revoked parent lease", err)
	}
	if err := s.ledger.Release(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if landed, err := s.LandPending(t.Context(), p); err != nil || len(landed) != 0 {
		t.Fatalf("stopped pending landed: %v %v", landed, err)
	}
	if read(t, canonical, "a") != "before" {
		t.Fatal("stopped child changed canonical")
	}
	if err := s.ledger.Update(t.Context(), func(tx *ledger.Tx) error { return task.CheckExecutionTx(tx, &token) }); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatal("stopped source remains valid")
	}
}

type stoppingNode struct {
	*localNode
	before func(ops.Kind)
}

func (n *stoppingNode) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if n.before != nil {
		n.before(req.Op)
	}
	return n.localNode.Artifact(ctx, node, req)
}

func TestCancellationBeforeApplyRefusesAndAfterApplySettlesWAL(t *testing.T) {
	for _, phase := range []ops.Kind{ops.Changed, ops.Apply} {
		t.Run(string(phase), func(t *testing.T) {
			canonical := t.TempDir()
			write(t, canonical, "a", "before")
			local := &localNode{root: t.TempDir(), state: t.TempDir()}
			s, p := newStore(t, local, project.Home{Node: "node", Path: canonical})
			nodes := &stoppingNode{localNode: local}
			s.nodes = nodes
			tasks, err := task.OpenLedger(s.ledger, "")
			if err != nil {
				t.Fatal(err)
			}
			tracked, _ := tasks.Create(task.Task{Channel: "c"})
			token, _ := tasks.ExecutionToken(tracked.ID)
			registry := execution.New(t.Context(), tasks)
			s.SetExecution(registry)
			ws, err := s.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "att"})
			if err != nil {
				t.Fatal(err)
			}
			write(t, ws.Path, "a", "after")
			result, _, err := s.Publish(t.Context(), ws, ws.Base, "att", "result")
			if err != nil {
				t.Fatal(err)
			}
			// Changed is reached on the node only for metadata-only projects. Use
			// the snapshot before landing as the pre-admission barrier otherwise.
			fired := false
			var wait execution.WaitSet
			nodes.before = func(op ops.Kind) {
				match := op == phase
				if phase == ops.Changed {
					match = op == ops.Snapshot
				}
				if fired || !match {
					return
				}
				fired = true
				ids, err := tasks.SetAside(tracked.ID, task.StateCancelled)
				if err != nil {
					t.Fatal(err)
				}
				if phase == ops.Apply {
					wait = registry.Stop(ids, task.ErrExecutionStopped)
				}
			}
			land, err := s.Land(t.Context(), p, result.ID, "test", Source{Execution: &token, AttemptID: "att"})
			if !fired {
				t.Fatal("test missed landing barrier")
			}
			if phase == ops.Changed {
				if !errors.Is(err, task.ErrExecutionStopped) || read(t, canonical, "a") != "before" {
					t.Fatalf("stopped before apply: %+v %v", land, err)
				}
			} else {
				if err != nil || land.State != LandCommitted || read(t, canonical, "a") != "after" {
					t.Fatalf("admitted WAL did not settle: %+v %v", land, err)
				}
				if err := wait.Wait(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
