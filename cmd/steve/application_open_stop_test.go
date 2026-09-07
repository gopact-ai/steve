package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestApplicationCancelsUnreceiptedOpenUsingNativeProof(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP agent: %v %s", err, output)
	}
	for _, mode := range []string{"missing", "created", "unreachable"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			server := node.NewServer(node.ServerConfig{Name: "worker", Listen: "127.0.0.1:0", Token: "unreceipted-open", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: node.CoordinatorSessionAuthorizer{}})
			nodeCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- server.Serve(nodeCtx) }()
			defer func() { stop(); <-done }()
			for server.Addr() == "127.0.0.1:0" {
				select {
				case <-ctx.Done():
					t.Fatal("node never listened")
				case <-time.After(time.Millisecond):
				}
			}
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			tasks, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Goal: "unreceipted preparation", Channel: "console:open-stop", Member: "mock", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts := attempt.New(book)
			r, err := attempts.Open(ctx, attempt.Spec{ID: "original-open", TaskID: tracked.ID, TurnID: "input-1", Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: "mock", Agent: "mock", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "workspace-1", Project: "p", Node: "worker", Path: t.TempDir(), Kind: project.KindWorktree}})
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(ctx, r.ID, attempt.Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			binding := harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "stop-cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: nodewire.SessionBinding{ProjectID: "p", SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, "mock"), TaskID: tracked.ID, AttemptID: r.ID, NodeID: "worker", ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: token.Epoch}, CommandID: attempt.InputCommandID(r)}
			connections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "unreceipted-open"}})
			defer connections.Close()
			authority := &applicationStopAuthority{epoch: 1}
			connections.SetSessionAuthorizer(func(ctx context.Context, target string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action string) error {
				if target != "worker" {
					return errors.New("wrong target")
				}
				return authority.AuthorizeNodeSession(ctx, "stop-cluster", a, b, action)
			})
			manager, _ := harness.NewManager(nil)
			manager.SetTransports(connections)
			manager.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
				return harness.WithNodeSession(ctx, binding), nil
			})
			defer manager.Stop()
			if mode == "created" {
				// The node accepts an open; the coordinator loses its receipt and
				// never commits the native ID or sends task input.
				_, err := connections.NodeSession(ctx, "worker", nodewire.SessionRequest{Action: "open", Authority: binding.Authority, Binding: binding.Binding, Harness: "mock", CommandID: binding.CommandID + "/open", Workdir: r.Workspace.Path})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := attempts.MarkUnsettled(ctx, r.ID, "lost-open", errors.New("open response missing"), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
				t.Fatal(err)
			}
			if mode == "unreachable" {
				connections.Close()
			}
			if mode == "missing" {
				proof, err := manager.ReconcileNodeOpen(ctx, harness.Placement{Node: "worker", Harness: "mock"}, r.Workspace.Path, true)
				if err != nil {
					t.Fatal(err)
				}
				for _, tamper := range []string{"action", "command", "task-epoch", "coordinator-epoch", "unconfirmed-stop", "session-id"} {
					bad := proof
					openProof := *proof.OpenReceipt
					bad.OpenReceipt = &openProof
					switch tamper {
					case "action":
						openProof.Action = "inspect-open"
					case "command":
						openProof.CommandID = "another/open"
					case "task-epoch":
						bad.Binding.TaskEpoch++
					case "coordinator-epoch":
						openProof.Authority.CoordinatorEpoch = 0
					case "unconfirmed-stop":
						bad.ProcessStopped = false
					case "session-id":
						bad.ID = "ns_another"
					}
					if _, err := attempts.ConfirmTaskStopped(ctx, r.ID, "tampered-proof", attempt.RetainedEvidence{ObservedAt: time.Now(), Session: bad}); err == nil {
						t.Fatalf("%s evidence retired original writer", tamper)
					}
					unsettled, _ := attempts.Get(ctx, r.ID)
					if !unsettled.Unsettled || unsettled.Session != "" || unsettled.StopEvidence != "" {
						t.Fatalf("%s evidence changed original preparation", tamper)
					}
				}
			}
			stops := newApplicationStops(attempts, tasks, manager)
			if err := stops.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := attempts.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unreachable" {
				if !current.Unsettled || current.StopEvidence != "" || current.Session != "" {
					t.Fatalf("offline node invented a cancellation proof: %+v", current)
				}
				return
			}
			if current.Unsettled || !current.State.Terminal() || current.StopEvidence == "" || current.Session == "" {
				t.Fatalf("native cancellation did not settle original attempt: %+v", current)
			}
			receipt, exists, err := attempts.TaskStopReceipt(ctx, r.ID)
			if err != nil || !exists || receipt.Evidence.Session.OpenReceipt == nil || receipt.Evidence.Session.OpenReceipt.CancelledBeforeOpen != (mode == "missing") {
				t.Fatalf("missing original open cancellation receipt: %+v %v", receipt, err)
			}
			if err := attempts.Renew(ctx, r.ID); err == nil {
				t.Fatal("cancelled original preparation retained writer authority")
			}
			if err := stops.Reconcile(ctx); err != nil {
				t.Fatalf("repeated reconciliation failed: %v", err)
			}
			final, _ := attempts.Get(ctx, r.ID)
			if final.Revision != current.Revision {
				t.Fatal("repeated cancellation changed settled attempt")
			}
		})
	}
}
