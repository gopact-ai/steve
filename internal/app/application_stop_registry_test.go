package app

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// Only the fault boundary is substituted: session creation, inspection and
// physical stopping still use the real node transport and isolated ACP peer.
type registryStopSessions struct {
	manager *harness.Manager
	calls   atomic.Int32
	inspect func(nodewire.SessionState) nodewire.SessionState
	stopped func(nodewire.SessionState) nodewire.SessionState
}

func (s *registryStopSessions) AttachRetainedSession(ctx context.Context, at harness.Placement, id, workdir string) (harness.ResumableRunner, error) {
	s.calls.Add(1)
	runner, err := s.manager.AttachRetainedSession(ctx, at, id, workdir)
	if err != nil {
		return nil, err
	}
	return registryStopRunner{ResumableRunner: runner, sessions: s}, nil
}

func (s *registryStopSessions) ReconcileNodeOpen(ctx context.Context, at harness.Placement, workdir string, cancel bool) (nodewire.SessionState, error) {
	s.calls.Add(1)
	return s.manager.ReconcileNodeOpen(ctx, at, workdir, cancel)
}

type registryStopRunner struct {
	harness.ResumableRunner
	sessions *registryStopSessions
}

func (r registryStopRunner) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	state, err := r.ResumableRunner.(harness.RetainedSessionInspector).InspectRetained(ctx)
	if err == nil && r.sessions.inspect != nil {
		state = r.sessions.inspect(state)
	}
	return state, err
}

func (r registryStopRunner) StopRetained(ctx context.Context) (nodewire.SessionState, error) {
	state, err := r.ResumableRunner.(harness.RetainedStopper).StopRetained(ctx)
	if err == nil && r.sessions.stopped != nil {
		state = r.sessions.stopped(state)
	}
	return state, err
}

type stopRegistryFixture struct {
	ctx      context.Context
	book     *ledger.Ledger
	tasks    *task.Store
	attempts *attempt.Service
	record   attempt.Record
	registry *execution.Registry
	owner    *execution.Scope
	sessions *registryStopSessions
	stops    *applicationStops
}

func newStopRegistryFixture(t *testing.T, bin string, pendingOpen bool) *stopRegistryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	server := node.NewServer(node.ServerConfig{
		Name: "worker", Listen: "127.0.0.1:0", Token: "registry-stop-test",
		StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses:         map[string]node.HarnessSpec{"mock": {Command: bin}},
		SessionAuthorizer: &applicationStopAuthority{epoch: 1},
	})
	nodeCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(nodeCtx) }()
	t.Cleanup(func() { stop(); <-served })
	for server.Addr() == "" || server.Addr() == "127.0.0.1:0" {
		select {
		case <-ctx.Done():
			t.Fatal("isolated node did not listen")
		case <-time.After(time.Millisecond):
		}
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Goal: "stop the original writer", Channel: "console:registry-stop", Member: "mock", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
		t.Fatal(err)
	}
	registry := execution.New(ctx, tasks)
	owner, err := registry.Begin(ctx, execution.Key{TaskID: tracked.ID, InstanceID: "original-turn", AttemptID: "original-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Finish(harness.ErrStopUnconfirmed) })
	attempts := attempt.New(book)
	record, err := attempts.Open(ctx, attempt.Spec{
		ID: "original-attempt", TaskID: tracked.ID, TurnID: "original-turn",
		Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: "mock", Agent: "mock",
		Execution: owner.Token(), Scope: attempt.ScopePathSet,
		Workspace: project.Workspace{ID: "original-workspace", Project: "p", Node: "worker", Path: t.TempDir(), Kind: project.KindWorktree},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(*record.Execution, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	record, err = attempts.Advance(ctx, record.ID, attempt.Prepared, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := harness.NodeSessionContext{
		Authority: nodewire.SessionAuthority{ClusterID: "stop-cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1},
		Binding:   sessionBinding(record, tracked), CommandID: attempt.InputCommandID(record),
	}
	connections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "registry-stop-test"}})
	t.Cleanup(connections.Close)
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetTransports(connections)
	manager.SetStopRegistrar(execution.RegisterStopHandler)
	if !pendingOpen {
		runner, err := manager.OpenSession(harness.WithNodeSession(ctx, binding), harness.Placement{Node: record.Node, Harness: record.Harness}, "", record.Workspace.Path, nil)
		if err != nil {
			t.Fatal(err)
		}
		record, err = attempts.Advance(ctx, record.ID, attempt.Running, "test", func(r *attempt.Record) { r.Session = runner.ID() })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := attempts.MarkUnsettled(ctx, record.ID, "test", harness.ErrStopUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	manager.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
		key, ok := execution.KeyOf(ctx)
		if !ok || key.TaskID != record.TaskID || key.AttemptID != record.ID || execution.Token(ctx) != nil {
			return nil, errors.New("stop must inspect the original attempt without readmission")
		}
		return harness.WithNodeSession(ctx, binding), nil
	})
	if _, err := tasks.SetAside(tracked.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	sessions := &registryStopSessions{manager: manager}
	stops := newApplicationStops(attempts, tasks, sessions)
	stops.executions = registry
	return &stopRegistryFixture{ctx: ctx, book: book, tasks: tasks, attempts: attempts, record: record, registry: registry, owner: owner, sessions: sessions, stops: stops}
}

func (f *stopRegistryFixture) requireBusy(t *testing.T) {
	t.Helper()
	if _, err := f.registry.SealIdle(); !errors.Is(err, execution.ErrBusy) {
		t.Fatalf("unresolved execution stopped guarding maintenance: %v", err)
	}
	if err := f.registry.WhileTaskIdle(f.record.TaskID, func() error {
		t.Error("unresolved execution permitted task completion")
		return nil
	}); !errors.Is(err, task.ErrCompleteBusy) {
		t.Fatalf("unresolved task admission guard = %v", err)
	}
}

func (f *stopRegistryFixture) requireResolved(t *testing.T) {
	t.Helper()
	if active := f.registry.Active(); len(active) != 0 {
		t.Fatalf("durably stopped and accounted execution still quarantines Registry: %v", active)
	}
	if err := f.registry.Stop([]string{f.record.TaskID}, task.ErrExecutionStopped).Wait(f.ctx); err != nil {
		t.Fatalf("later stop still reports obsolete uncertainty: %v", err)
	}
	if err := f.registry.WhileTaskIdle(f.record.TaskID, func() error { return nil }); err != nil {
		t.Fatalf("old execution still blocks task completion: %v", err)
	}
	release, err := f.registry.SealIdle()
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Resolution does not revive the old token. New execution must acquire
	// the explicitly resumed task's epoch, and can reuse the retired workspace.
	if err := f.tasks.CheckExecution(*f.record.Execution); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("stop resolution upgraded the original token: %v", err)
	}
	if _, err := f.tasks.Advance(f.record.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Begin(f.record.TaskID, "mock", "worker", ""); err != nil {
		t.Fatal(err)
	}
	next, err := f.registry.Begin(f.ctx, execution.Key{TaskID: f.record.TaskID, InstanceID: "next-turn", AttemptID: "next-attempt"})
	if err != nil {
		t.Fatalf("new epoch cannot execute after confirmed stop: %v", err)
	}
	defer next.Finish(nil)
	spec := f.record.Spec
	spec.ID, spec.TurnID, spec.Execution = "next-attempt", "next-turn", next.Token()
	if _, err := f.attempts.Open(f.ctx, spec); err != nil {
		t.Fatalf("confirmed old stop still blocks workspace admission: %v", err)
	}
}

func TestApplicationStopRegistryConvergesOnlyAfterDurableSettlement(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	for _, mode := range []string{"confirmed", "unreceipted-open", "owner-finishes-late", "open-owner-finishes-late", "settlement-retry", "wrong-inspection", "wrong-stop-session", "wrong-stop-task-epoch", "wrong-stop-attempt"} {
		t.Run(mode, func(t *testing.T) {
			pendingOpen := mode == "unreceipted-open" || mode == "open-owner-finishes-late"
			lateOwner := strings.HasSuffix(mode, "owner-finishes-late")
			f := newStopRegistryFixture(t, bin, pendingOpen)
			unresolved := error(&execution.RetainedObserverDetached{AttemptID: f.record.ID, NodeID: f.record.Node, SessionID: f.record.Session, Cause: harness.ErrStopUnconfirmed})
			if pendingOpen {
				unresolved = &execution.NodePreparationObserverDetached{AttemptID: f.record.ID, NodeID: f.record.Node, OpenCommandID: attempt.InputCommandID(f.record) + "/open", Cause: harness.ErrStopUnconfirmed}
			}
			if !lateOwner {
				f.owner.Finish(unresolved)
			}
			f.requireBusy(t)
			switch mode {
			case "settlement-retry":
				if _, err := f.book.DB().Exec(`CREATE TRIGGER reject_registry_settlement BEFORE INSERT ON bindings WHEN NEW.kind = 'task-attempt' BEGIN SELECT RAISE(FAIL, 'isolated accounting failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "wrong-inspection":
				f.sessions.inspect = func(state nodewire.SessionState) nodewire.SessionState {
					state.Binding.AttemptID = "another-attempt"
					return state
				}
			case "wrong-stop-session", "wrong-stop-task-epoch", "wrong-stop-attempt":
				f.sessions.stopped = func(state nodewire.SessionState) nodewire.SessionState {
					switch mode {
					case "wrong-stop-session":
						state.ID = "ns_another"
					case "wrong-stop-task-epoch":
						state.Binding.TaskEpoch++
					case "wrong-stop-attempt":
						state.Binding.AttemptID = "another-attempt"
					}
					return state
				}
			}
			err := f.stops.Reconcile(f.ctx)
			current, loadErr := f.attempts.Get(f.ctx, f.record.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if strings.HasPrefix(mode, "wrong-") {
				if err != nil {
					t.Fatal(err)
				}
				if !current.Unsettled || current.StopEvidence != "" {
					t.Fatalf("wrong identity was accepted: %+v", current)
				}
				if _, exists, err := f.attempts.TaskStopReceipt(f.ctx, current.ID); err != nil || exists {
					t.Fatalf("wrong identity persisted stop evidence: exists=%v err=%v", exists, err)
				}
				f.requireBusy(t)
				if err := f.registry.Stop([]string{current.TaskID}, task.ErrExecutionStopped).Wait(f.ctx); !errors.Is(err, harness.ErrStopUnconfirmed) {
					t.Fatalf("wrong evidence erased unresolved error: %v", err)
				}
				return
			}
			if current.Unsettled || current.StopEvidence != "task-stop/"+current.ID {
				t.Fatalf("native evidence was not durably accepted: %+v", current)
			}
			if mode == "settlement-retry" {
				if err == nil || !strings.Contains(err.Error(), "accounting remains pending") {
					t.Fatalf("settlement failure was hidden: %v", err)
				}
				f.requireBusy(t)
				row, _ := f.tasks.Get(current.TaskID)
				if !row.Attempts[0].Open() {
					t.Fatal("failed durable settlement updated task accounting")
				}
				if _, err := f.book.DB().Exec(`DROP TRIGGER reject_registry_settlement`); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "confirmed" || mode == "unreceipted-open" {
				if active := f.registry.Active(); len(active) != 0 {
					t.Fatalf("successful settlement did not immediately resolve its finished owner: %v", active)
				}
			}
			if lateOwner {
				f.requireBusy(t)
				f.owner.Finish(unresolved)
				// Already-accounted records must retry only the in-memory
				// projection, not write accounting or re-contact the node.
				if _, err := f.book.DB().Exec(`CREATE TRIGGER reject_duplicate_settlement BEFORE INSERT ON bindings WHEN NEW.kind = 'task-attempt' BEGIN SELECT RAISE(FAIL, 'accounting must not be repeated'); END`); err != nil {
					t.Fatal(err)
				}
			}
			calls := f.sessions.calls.Load()
			if err := f.stops.Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			if f.sessions.calls.Load() != calls {
				t.Fatal("settlement retry resent native stopping")
			}
			if lateOwner {
				if _, err := f.book.DB().Exec(`DROP TRIGGER reject_duplicate_settlement`); err != nil {
					t.Fatal(err)
				}
			}
			settled, _ := f.tasks.Get(current.TaskID)
			if settled.Attempts[0].Open() || settled.Attempts[0].UsageKnown == nil {
				t.Fatal("registry resolution preceded durable accounting")
			}
			f.requireResolved(t)
		})
	}

	t.Run("new-epoch-during-stop-confirmation", func(t *testing.T) {
		f := newStopRegistryFixture(t, bin, false)
		f.owner.Finish(harness.ErrStopUnconfirmed)
		received, release := make(chan struct{}), make(chan struct{})
		f.sessions.stopped = func(state nodewire.SessionState) nodewire.SessionState {
			close(received)
			select {
			case <-release:
			case <-f.ctx.Done():
			}
			return state
		}
		done := make(chan error, 1)
		go func() { done <- f.stops.Reconcile(f.ctx) }()
		select {
		case <-received:
		case <-f.ctx.Done():
			t.Fatal("original native stop never reached confirmation barrier")
		}
		if _, err := f.tasks.Finish(f.record.TaskID, task.OutcomeInterrupted, task.Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.tasks.Advance(f.record.TaskID, task.StateRunning); err != nil {
			t.Fatal(err)
		}
		if _, err := f.tasks.Begin(f.record.TaskID, "mock", "worker", ""); err != nil {
			t.Fatal(err)
		}
		next, err := f.registry.Begin(f.ctx, execution.Key{TaskID: f.record.TaskID, InstanceID: "new-turn", AttemptID: "new-attempt"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.tasks.BindAttempt(*next.Token(), "new-attempt", "new-turn"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { next.Finish(nil) })
		before, _ := f.tasks.Get(f.record.TaskID)
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		after, _ := f.tasks.Get(f.record.TaskID)
		if !reflect.DeepEqual(before.Attempts[1], after.Attempts[1]) || after.State != task.StateRunning || next.Context().Err() != nil {
			t.Fatal("old stop confirmation changed the new epoch's execution")
		}
		if active := f.registry.Active(); !reflect.DeepEqual(active, []string{"new-turn"}) {
			t.Fatalf("resolve missed the original or cleared the concurrent new owner: %v", active)
		}
		f.requireBusy(t)
		next.Finish(harness.ErrStopUnconfirmed)
		if err := f.stops.Reconcile(f.ctx); err != nil {
			t.Fatal(err)
		}
		if active := f.registry.Active(); !reflect.DeepEqual(active, []string{"new-turn"}) {
			t.Fatalf("replayed old confirmation erased new epoch uncertainty: %v", active)
		}
	})
}
