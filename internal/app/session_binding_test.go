package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Only the observation is changed: an incorrect preflight must not reach the
// real node's cancel/abort endpoint, even if a later receipt would be rejected.
type stopBindingTransport struct {
	*node.Registry
	observedImportID *string
	stops            atomic.Int32
}

func (c *stopBindingTransport) NodeSession(ctx context.Context, target string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if req.Action == nodewire.SessionActionCancel || req.Action == nodewire.SessionActionAbort {
		c.stops.Add(1)
	}
	state, err := c.Registry.NodeSession(ctx, target, req)
	if err == nil && req.Action == nodewire.SessionActionAttach && c.observedImportID != nil {
		state.Binding.NativeImportID = *c.observedImportID
	}
	return state, err
}

func stopBindingImport(t *testing.T, stateDir, workdir string) *nativehistory.Reference {
	t.Helper()
	source := nativehistory.Source{Harness: harness.Codex, Home: t.TempDir()}
	path := filepath.Join(source.Home, "sessions", "2026", "09", "19", "rollout-selected.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": "selected-native", "cwd": workdir}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := nativehistory.List(t.Context(), source)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list isolated history: %+v %v", entries, err)
	}
	ref, err := nativehistory.Snapshot(t.Context(), filepath.Join(stateDir, "native-imports"), nativehistory.ImportRequest{
		CommandID: "selected-import", Source: source, NativeID: entries[0].NativeID, Revision: entries[0].Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &ref
}

func TestApplicationStopsUseCommittedSessionBinding(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, output)
	}
	for _, tc := range []struct {
		name             string
		imported         bool
		wrongObservation bool
		observedImportID string
		stopState        task.State
	}{
		{name: "ordinary"},
		{name: "imported", imported: true},
		{name: "ordinary-cancelled", stopState: task.StateCancelled},
		{name: "imported-cancelled", imported: true, stopState: task.StateCancelled},
		{name: "imported-missing-id", imported: true, wrongObservation: true},
		{name: "imported-other-id", imported: true, wrongObservation: true, observedImportID: "import_" + strings.Repeat("f", 64)},
		{name: "ordinary-unexpected-id", wrongObservation: true, observedImportID: "import_" + strings.Repeat("f", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stopState == "" {
				tc.stopState = task.StatePaused
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			runtime, err := cluster.Open(cluster.Config{
				LedgerDir: t.TempDir(), PollInterval: 20 * time.Millisecond,
				Coordination: coordination.Config{ClusterID: "stop-cluster", NodeID: "coordinator", DataDir: t.TempDir(), Bootstrap: true, FailureDomain: "isolated-test", StorageLevel: "restricted"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := runtime.Close(); err != nil {
					t.Error(err)
				}
			}()
			active, err := runtime.WaitReady(ctx)
			if err != nil {
				t.Fatal(err)
			}
			stateDir, workdir := t.TempDir(), t.TempDir()
			var imported *nativehistory.Reference
			if tc.imported {
				imported = stopBindingImport(t, stateDir, workdir)
			}
			server := node.NewServer(node.ServerConfig{
				Name: "worker", Listen: "127.0.0.1:0", Token: "binding-test", StateDir: stateDir, WorkspaceRoot: workdir,
				Harnesses:         map[string]node.HarnessSpec{harness.Codex: {Command: bin, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + t.TempDir()}}},
				SessionAuthorizer: &applicationStopAuthority{epoch: active.Assignment.Epoch},
			})
			nodeCtx, stopNode := context.WithCancel(ctx)
			served := make(chan error, 1)
			go func() { served <- server.Serve(nodeCtx) }()
			defer func() { stopNode(); <-served }()
			for server.Addr() == "127.0.0.1:0" {
				select {
				case err := <-served:
					served <- err
					t.Fatalf("node exited before listening: %v", err)
				case <-ctx.Done():
					t.Fatal("node never listened")
				case <-time.After(time.Millisecond):
				}
			}
			tasks, err := task.OpenLedger(active.Ledger)
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Goal: "stop original input", Channel: "console:binding-stop", Member: "worker", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(tracked.ID, "worker", "worker", ""); err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts := attempt.New(active.Ledger)
			record, err := attempts.Open(ctx, attempt.Spec{
				ID: "original-attempt", TaskID: tracked.ID, TurnID: "original-turn", NativeCommandID: "original-native-input",
				ExecutionGeneration: 7, Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: harness.Codex, Agent: "worker",
				Execution: &token, NativeImport: imported, Scope: attempt.ScopePathSet,
				Workspace: project.Workspace{ID: "original-workspace", Project: "p", Node: "worker", Path: workdir, Kind: project.KindWorktree},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err := attempts.Advance(ctx, record.ID, attempt.Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			scope, err := execution.New(ctx, tasks).Begin(ctx, execution.Key{TaskID: tracked.ID, AttemptID: record.ID, InstanceID: record.TurnID})
			if err != nil {
				t.Fatal(err)
			}
			defer scope.Finish(harness.ErrStopUnconfirmed)
			connections := node.NewRegistry("stop-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "binding-test"}})
			defer connections.Close()
			first, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Stop()
			first.SetTransports(connections)
			first.SetNodeSessionBinder(newApplicationSessionBinder(active))
			observer, detach := context.WithCancel(scope.Context())
			defer detach()
			place := harness.Placement{Node: record.Node, Harness: record.Harness}
			runner, err := first.OpenSession(observer, place, "", workdir, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err = attempts.Advance(ctx, record.ID, attempt.Running, "test", func(r *attempt.Record) { r.Session = runner.ID() })
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
			case err := <-detached:
				t.Fatalf("original input ended before asking: %v", err)
			case <-ctx.Done():
				t.Fatal("original input never reached its native question")
			}
			if _, err := tasks.SetAside(tracked.ID, tc.stopState); err != nil {
				t.Fatal(err)
			}
			detach()
			if err := <-detached; !errors.Is(err, harness.ErrStopUnconfirmed) {
				t.Fatalf("observer loss fabricated stop evidence: %v", err)
			}
			first.Stop()

			link := &stopBindingTransport{Registry: connections}
			if tc.wrongObservation {
				link.observedImportID = &tc.observedImportID
			}
			next, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Stop()
			next.SetTransports(link)
			next.SetNodeSessionBinder(newApplicationSessionBinder(active))
			next.SetStopRegistrar(execution.RegisterStopHandler)
			stops := newApplicationStops(attempts, tasks, next)
			inspect := func() nodewire.SessionState {
				t.Helper()
				probe := execution.WithProbeKey(ctx, execution.Key{TaskID: record.TaskID, AttemptID: record.ID})
				bound, err := newApplicationSessionBinder(active)(probe, place, record.Session, workdir)
				if err != nil {
					t.Fatal(err)
				}
				binding, ok := harness.NodeSessionFromContext(bound)
				if !ok {
					t.Fatal("inspection lacks committed identity")
				}
				state, err := connections.NodeSession(ctx, record.Node, nodewire.SessionRequest{
					Action: nodewire.SessionActionAttach, ID: record.Session, Authority: binding.Authority, Binding: binding.Binding, CommandID: binding.CommandID,
				})
				if err != nil {
					t.Fatal(err)
				}
				if state.InputAccepted != 1 || state.Command == nil || state.Command.ID != record.NativeCommandID || state.Binding.NativeImportID != record.NativeImportID() {
					t.Fatalf("original native input or provenance changed: %+v", state)
				}
				if imported != nil && (state.NativeImport == nil || state.NativeImport.ID != imported.ID || len(state.Questions) != 1 || state.Questions[0].Question.SessionID != imported.NativeID) {
					t.Fatalf("agent did not load the selected native history: %+v", state)
				}
				return state
			}
			if err := stops.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.wrongObservation {
				state := inspect()
				pending, err := attempts.Get(ctx, record.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, exists, err := attempts.TaskStopReceipt(ctx, record.ID)
				if err != nil || exists || !pending.Unsettled || pending.StopEvidence != "" || link.stops.Load() != 0 || state.Command.Settled || state.ProcessStopped {
					t.Fatalf("mismatched import observation stopped original execution: stops=%d pending=%+v native=%+v receipt=%v err=%v", link.stops.Load(), pending, state, exists, err)
				}
				link.observedImportID = nil
				if err := stops.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			state := inspect()
			stored, err := attempts.Get(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt, exists, err := attempts.TaskStopReceipt(ctx, record.ID)
			if err != nil || !exists || stored.Unsettled || stored.State != attempt.Failed || stored.SessionSettled == nil || !*stored.SessionSettled || stored.StopEvidence != "task-stop/"+record.ID || !state.Command.Settled || link.stops.Load() == 0 {
				t.Fatalf("committed binding did not stop original execution: stops=%d record=%+v native=%+v receipt=%+v err=%v", link.stops.Load(), stored, state, receipt, err)
			}
			if receipt.Evidence.Session.Binding != state.Binding {
				t.Fatal("stop receipt lost original execution identity")
			}
			accounted, ok := tasks.Get(tracked.ID)
			if !ok || accounted.State != tc.stopState || len(accounted.Attempts) != 1 || accounted.Attempts[0].Open() || accounted.Attempts[0].ExecutionID != record.ID {
				t.Fatalf("confirmed stop did not settle original accounting: %+v", accounted)
			}
			before := link.stops.Load()
			if err := stops.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if link.stops.Load() != before {
				t.Fatal("settled stop replayed a native mutation")
			}
		})
	}
}
