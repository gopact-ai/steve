package app

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/gopact-ai/steve/internal/view"
)

type applicationStopAuthority struct {
	mu       sync.Mutex
	epoch    uint64
	mutating []nodewire.SessionAction
}

func (a *applicationStopAuthority) AuthorizeNodeSession(_ context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if principal != "stop-cluster" || authority.ClusterID != principal || authority.CoordinatorEpoch != a.epoch || authority.WriterGeneration != a.epoch || binding.NodeID != "worker" {
		return errors.New("stale coordinator or wrong authenticated node")
	}
	if action == "prompt" || action == "cancel" || action == "abort" || action == "start" {
		a.mutating = append(a.mutating, action)
	}
	return nil
}

type applicationStopConnection struct {
	manager *harness.Manager
	offline bool
}

func (c *applicationStopConnection) AttachRetainedSession(ctx context.Context, place harness.Placement, id, workdir string) (harness.ResumableRunner, error) {
	if c.offline {
		return nil, errors.New("original worker is temporarily offline")
	}
	return c.manager.AttachRetainedSession(ctx, place, id, workdir)
}

func TestApplicationStopsRecoverDurableRevocationWithoutReplayingNativeInput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated test agent: %v %s", err, output)
	}
	for _, mode := range []string{"paused", "cancelled", "parent-stopped", "offline-then-returned", "resumed-accounting", "accounting-retry", "healthy-handoff", "normal-done"} {
		t.Run(mode, func(t *testing.T) {
			bounded, end := context.WithTimeout(t.Context(), 20*time.Second)
			defer end()
			authority := &applicationStopAuthority{epoch: 1}
			nodeCtx, closeNode := context.WithCancel(t.Context())
			server := node.NewServer(node.ServerConfig{Name: "worker", Listen: "127.0.0.1:0", Token: "stop-test", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: authority})
			served := make(chan error, 1)
			go func() { served <- server.Serve(nodeCtx) }()
			defer func() { closeNode(); <-served }()
			for server.Addr() == "" {
				select {
				case <-bounded.Done():
					t.Fatal("test node never listened")
				case <-time.After(time.Millisecond):
				}
			}
			bookDir := t.TempDir()
			book, err := ledger.Open(bookDir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = book.Close() }()
			tasks, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			member := "mock"
			if mode == "parent-stopped" {
				member = "supervisor"
			}
			tracked, err := tasks.Create(task.Task{Goal: "original task", Channel: "console:stopping", Member: member, ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			stopTask := tracked.ID
			if mode == "parent-stopped" {
				tracked, err = tasks.Spawn(tracked.ID, task.Task{Goal: "original child", Channel: "console:stopping", Member: "mock", ProjectID: "p"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts := attempt.New(book)
			record, err := attempts.Open(bounded, attempt.Spec{ID: "original-attempt", TaskID: tracked.ID, TurnID: "original-input", Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: "mock", Agent: "mock", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original-workspace", Project: "p", Node: "worker", Path: t.TempDir(), Kind: project.KindWorktree}})
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err = attempts.Advance(bounded, record.ID, attempt.Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			binding := harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "stop-cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: nodewire.SessionBinding{ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: token.Epoch}, CommandID: attempt.InputCommandID(record)}
			connections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "stop-test"}})
			first, _ := harness.NewManager(nil)
			first.SetTransports(connections)
			defer connections.Close()
			defer first.Stop()
			observer, detach := context.WithCancel(harness.WithNodeSession(bounded, binding))
			defer detach()
			runner, err := first.OpenSession(observer, harness.Placement{Node: "worker", Harness: "mock"}, "", record.Workspace.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err = attempts.Advance(bounded, record.ID, attempt.Running, "test", func(r *attempt.Record) { r.Session = runner.ID() })
			if err != nil {
				t.Fatal(err)
			}
			asked, detached := make(chan struct{}), make(chan error, 1)
			go func() {
				_, _, err := runner.(harness.TurnRunner).PromptTurn(observer, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
					close(asked)
					<-ctx.Done()
					return view.Answer{}, ctx.Err()
				}, nil)
				detached <- err
			}()
			select {
			case <-asked:
			case <-bounded.Done():
				t.Fatal("original task did not reach native question")
			}
			wantStop := mode != "healthy-handoff" && mode != "normal-done"
			if mode == "resumed-accounting" || mode == "accounting-retry" {
				if err := attempts.MarkUnsettled(bounded, record.ID, "test", errors.New("source observation ended after known usage"), &attempt.Usage{Input: 100, Output: 50, Model: "known-model", Reported: true}); err != nil {
					t.Fatal(err)
				}
			}
			if wantStop {
				state := task.StatePaused
				if mode == "cancelled" {
					state = task.StateCancelled
				}
				if _, err := tasks.SetAside(stopTask, state); err != nil {
					t.Fatal(err)
				}
				if mode == "resumed-accounting" {
					if _, err := tasks.FinishAs(tracked.ID, task.OutcomeInterrupted, task.Tokens{Input: 10, Output: 5, Total: 15}, 0, "known-model"); err != nil {
						t.Fatal(err)
					}
					if _, err := tasks.Advance(tracked.ID, task.StateRunning); err != nil {
						t.Fatal(err)
					}
					if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
						t.Fatal(err)
					}
					current, err := tasks.ExecutionToken(tracked.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := tasks.BindAttempt(current, "next-accounting-attempt", "later-input"); err != nil {
						t.Fatal(err)
					}
				}
			} else if mode == "normal-done" {
				if _, err := tasks.Advance(tracked.ID, task.StateDone); err != nil {
					t.Fatal(err)
				}
			}
			// The first coordinator ends after persisting SetAside, before
			// any in-memory Registry.Stop handler could send a node request.
			detach()
			if err := <-detached; !errors.Is(err, harness.ErrStopUnconfirmed) {
				t.Fatalf("lost observer fabricated stop evidence: %v", err)
			}
			first.Stop()
			connections.Close()
			if err := book.Close(); err != nil {
				t.Fatal(err)
			}
			book, err = ledger.Open(bookDir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			attempts = attempt.New(book)
			tasks, err = task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			authority.mu.Lock()
			authority.epoch = 2
			authority.mu.Unlock()
			binding.Authority.CoordinatorEpoch, binding.Authority.WriterGeneration = 2, 2
			nextConnections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "stop-test"}})
			defer nextConnections.Close()
			next, _ := harness.NewManager(nil)
			next.SetTransports(nextConnections)
			next.SetStopRegistrar(execution.RegisterStopHandler)
			next.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
				key, ok := execution.KeyOf(ctx)
				if !ok || key.AttemptID != record.ID || key.TaskID != record.TaskID || execution.Token(ctx) != nil {
					return nil, errors.New("stopping did not use the original read-only probe")
				}
				return harness.WithNodeSession(ctx, binding), nil
			})
			defer next.Stop()
			link := &applicationStopConnection{manager: next, offline: mode == "offline-then-returned"}
			consumer := newApplicationStops(attempts, tasks, link)
			if mode == "accounting-retry" {
				if _, err := book.DB().Exec(`CREATE TRIGGER reject_stop_accounting BEFORE UPDATE OF data ON bindings WHEN NEW.kind = 'document' AND NEW.id = 'tasks' BEGIN SELECT RAISE(FAIL, 'task accounting unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			firstErr := consumer.Reconcile(bounded)
			if mode == "accounting-retry" {
				if firstErr == nil || !strings.Contains(firstErr.Error(), "accounting remains pending") {
					t.Fatalf("failed usage projection was hidden: %v", firstErr)
				}
				settled, _ := attempts.Get(bounded, record.ID)
				if settled.Unsettled || settled.StopEvidence == "" {
					t.Fatal("accounting failure changed confirmed native stopping into uncertainty")
				}
				authority.mu.Lock()
				before := len(authority.mutating)
				authority.mu.Unlock()
				if _, err := book.DB().Exec(`DROP TRIGGER reject_stop_accounting`); err != nil {
					t.Fatal(err)
				}
				if err := consumer.Reconcile(bounded); err != nil {
					t.Fatal(err)
				}
				authority.mu.Lock()
				after := len(authority.mutating)
				authority.mu.Unlock()
				if before != after {
					t.Fatal("retrying accounting resent native stopping")
				}
			} else if firstErr != nil {
				t.Fatal(firstErr)
			}
			if link.offline {
				pending, err := attempts.Get(bounded, record.ID)
				if err != nil || !pending.Unsettled || !strings.Contains(pending.Error, "尚未收到原节点的停止确认") || pending.StopEvidence != "" {
					t.Fatalf("offline stop was not visibly pending: %+v %v", pending, err)
				}
				before, err := book.Events(bounded, record.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := consumer.Reconcile(bounded); err != nil {
					t.Fatal(err)
				}
				after, err := book.Events(bounded, record.ID)
				if err != nil || len(after) != len(before) {
					t.Fatalf("unchanged offline state kept adding stop events: %d/%d %v", len(before), len(after), err)
				}
				link.offline = false
				if err := consumer.Reconcile(bounded); err != nil {
					t.Fatal(err)
				}
			}
			st, err := nextConnections.NodeSession(bounded, "worker", nodewire.SessionRequest{Action: "attach", ID: record.Session, Authority: binding.Authority, Binding: binding.Binding, CommandID: binding.CommandID})
			if err != nil || st.InputAccepted != 1 || st.Command == nil || st.Command.ID != "original-input" {
				t.Fatalf("original input receipt changed: %+v %v", st, err)
			}
			stored, err := attempts.Get(bounded, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantStop {
				receipt, ok, err := attempts.TaskStopReceipt(bounded, record.ID)
				if err != nil || !ok || receipt.Evidence.Session.ID != runner.ID() || stored.Unsettled || stored.State != attempt.Failed || !st.Command.Settled {
					t.Fatalf("new coordinator did not confirm original stop: record=%+v receipt=%+v err=%v", stored, receipt, err)
				}
				accounted, _ := tasks.Get(record.TaskID)
				if len(accounted.Attempts) == 0 || accounted.Attempts[0].Open() || accounted.Attempts[0].ExecutionID != record.ID {
					t.Fatalf("original accounting row was not settled: %+v", accounted)
				}
				if mode == "resumed-accounting" || mode == "accounting-retry" {
					if accounted.Budget.Tokens.Total != 150 || accounted.Attempts[0].Tokens.Total != 150 {
						t.Fatalf("known old usage was lost or double charged: %+v", accounted)
					}
				}
				if mode == "resumed-accounting" && (accounted.State != task.StateRunning || len(accounted.Attempts) != 2 || !accounted.Attempts[1].Open() || accounted.Attempts[1].ExecutionID != "next-accounting-attempt" || accounted.Budget.Turns != 2) {
					t.Fatalf("late original receipt closed a later turn: %+v", accounted)
				}
			} else if stored.State != attempt.Running || st.Command.Settled || st.ProcessStopped || len(st.Questions) != 1 || st.Questions[0].State != "pending" {
				t.Fatalf("healthy handoff/normal Done stopped native work: record=%+v state=%+v", stored, st)
			}
			authority.mu.Lock()
			before := len(authority.mutating)
			authority.mu.Unlock()
			if err := consumer.Reconcile(bounded); err != nil {
				t.Fatal(err)
			}
			authority.mu.Lock()
			after := len(authority.mutating)
			authority.mu.Unlock()
			if after != before {
				t.Fatal("repeat scanner replayed a native mutation")
			}
		})
	}
}
