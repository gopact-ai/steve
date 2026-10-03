package turn

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type exactMachineAuthority struct {
	binding nodewire.SessionBinding
	receipt nodewire.SessionReceipt
}

func (a *exactMachineAuthority) AuthorizeNodeSession(_ context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, _ nodewire.SessionAction) error {
	if principal != "owned-cluster" || authority.ClusterID != principal || authority.CoordinatorEpoch != 1 || authority.WriterGeneration != 1 || binding != a.binding {
		return errors.New("native fixture exact authority differs")
	}
	return nil
}
func (a *exactMachineAuthority) AuthorizeNodeReceipt(_ context.Context, principal string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	if principal != "owned-cluster" || authority.ClusterID != principal || receipt != a.receipt || receipt.Validate() != nil {
		return errors.New("native fixture exact receipt differs")
	}
	return nil
}

// The fixture uses the public node server and authenticated loopback transport.
type ownedRecoveryMachine struct {
	registry *node.Registry
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
	t        *testing.T
}

func startOwnedRecoveryMachine(t *testing.T, cfg node.ServerConfig) (*ownedRecoveryMachine, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	cfg.Listener, cfg.Token = listener, "owned-machine-token"
	server := node.NewServer(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	m := &ownedRecoveryMachine{registry: node.NewRegistry("owned-cluster", map[string]node.Config{cfg.Name: {Addr: listener.Addr().String(), Token: cfg.Token}}), cancel: cancel, done: make(chan error, 1), t: t}
	go func() { m.done <- server.Serve(ctx) }()
	t.Cleanup(m.Close)
	return m, nil
}

func (m *ownedRecoveryMachine) Do(ctx context.Context, principal string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if principal != "owned-cluster" {
		return nodewire.SessionState{}, errors.New("wrong machine principal")
	}
	return m.registry.NodeSession(ctx, req.Binding.NodeID, req)
}
func (m *ownedRecoveryMachine) Ack(ctx context.Context, principal string, req nodewire.SessionReceiptRequest) error {
	if principal != "owned-cluster" {
		return errors.New("wrong machine receipt principal")
	}
	return m.registry.AcknowledgeNodeReceipt(ctx, req.Receipt.Binding.NodeID, req)
}
func (m *ownedRecoveryMachine) Close() {
	m.once.Do(func() {
		m.registry.Close()
		m.cancel()
		select {
		case err := <-m.done:
			if err != nil {
				m.t.Errorf("owned machine shutdown: %v", err)
			}
		case <-time.After(waitDeadline):
			m.t.Error("owned machine did not finish")
		}
	})
}

type actualMachineTransport struct{ machine *ownedRecoveryMachine }

func (t *actualMachineTransport) Transport(string, string) acphost.Transport { return nil }
func (t *actualMachineTransport) NodeSession(ctx context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return t.machine.Do(ctx, "owned-cluster", req)
}

func TestRecoveryMachineProofAcrossHubRefusalAndNodeRestart(t *testing.T) {
	for _, mode := range []string{"hub-refused-close-proof", "node-restart-before-close"} {
		t.Run(mode, func(t *testing.T) {
			c, p, original, ws := sharedCopy(t)
			spec := recoveryCopySpec(t, c, p, ws, "actual-native-copy")
			writer, err := c.attempts.Open(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.tasks.BindAttempt(*writer.Execution, writer.ID, writer.TurnID); err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.Advance(t.Context(), writer.ID, attempt.Prepared, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			tracked, found := c.tasks.Get(writer.TaskID)
			if !found {
				t.Fatal("original task is missing")
			}
			binding := nodewire.SessionBinding{ProjectID: p.ID, SessionID: attempt.RetainedSessionID(tracked.Channel, writer.TaskID, writer.Agent), TaskID: writer.TaskID, AttemptID: writer.ID, NodeID: writer.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(writer), TaskEpoch: writer.Execution.Epoch}
			verifier := &exactMachineAuthority{binding: binding}
			bin := filepath.Join(t.TempDir(), "mockagent")
			build := exec.Command("go", "build", "-o", bin, "./cmd/mockagent")
			build.Dir = "../.."
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("native fixture build: %v %s", err, out)
			}
			cfg := node.ServerConfig{Name: writer.Node, StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: verifier}
			machine, err := startOwnedRecoveryMachine(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { machine.Close() }()
			req := nodewire.SessionRequest{Action: nodewire.SessionActionOpen, Authority: nodewire.SessionAuthority{ClusterID: "owned-cluster", CoordinatorNodeID: "owned-hub", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: binding, Harness: writer.Harness, Workdir: ws.Path, CommandID: attempt.InputCommandID(writer) + "/open"}
			opened, err := machine.Do(t.Context(), "owned-cluster", req)
			if err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.RecordSession(t.Context(), writer.ID, "fixture", opened.ID)
			if err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.Advance(t.Context(), writer.ID, attempt.Running, "fixture", func(r *attempt.Record) { r.NativeContext = opened.ContextID })
			if err != nil {
				t.Fatal(err)
			}
			req.ID, req.Action, req.CommandID, req.InputSequence, req.Text = opened.ID, nodewire.SessionActionPrompt, attempt.InputCommandID(writer), 1, "answer"
			settled, err := machine.Do(t.Context(), "owned-cluster", req)
			if err != nil {
				t.Fatal(err)
			}
			pollCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for settled.Command == nil || !settled.Command.Settled {
				poll := req
				poll.Action, poll.After, poll.WaitMS = nodewire.SessionActionPoll, settled.Sequence, 50
				settled, err = machine.Do(pollCtx, "owned-cluster", poll)
				if err != nil {
					t.Fatal(err)
				}
			}
			verifier.receipt = settled.Command.Receipt
			if err := machine.Ack(t.Context(), "owned-cluster", nodewire.SessionReceiptRequest{Authority: req.Authority, Receipt: verifier.receipt}); err != nil {
				t.Fatal(err)
			}
			if err := c.attempts.MarkSessionSettled(t.Context(), writer.ID, "fixture"); err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.Get(t.Context(), writer.ID)
			if err != nil {
				t.Fatal(err)
			}
			completion, _, err := c.completion(t.Context(), writer, Result{Text: "native unchanged"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.FinishCompletion(t.Context(), writer.ID, "fixture", completion)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.store.SaveSession(state.Session{ConversationID: tracked.Channel, AgentID: writer.Agent, HarnessID: writer.Harness, NodeID: writer.Node, UpstreamID: writer.Session, Workspace: ws.Path, ProjectID: p.ID}); err != nil {
				t.Fatal(err)
			}
			original = retireOriginalRecoverySource(t, c, original)
			transport := &actualMachineTransport{machine: machine}
			manager, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()
			manager.SetTransports(transport)
			manager.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, id, workdir string) (context.Context, error) {
				if id != writer.Session || workdir != ws.Path {
					return nil, errors.New("close fixture placement differs")
				}
				return harness.WithNodeSession(ctx, harness.NodeSessionContext{Authority: req.Authority, Binding: binding, CommandID: attempt.InputCommandID(writer)}), nil
			})
			err = c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), original.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
				enrollStoppedOriginals(t, ctx, c, original.Abandoned.WorkspaceRecoveryID, driver)
				episode, err := c.attempts.BeginRecoveryDrain(ctx, original.Abandoned.WorkspaceRecoveryID, driver)
				if err != nil {
					return err
				}
				var native attempt.RecoveryNative
				for _, n := range episode.NativeRetirements {
					if n.Copy {
						native = n
					}
				}
				if native.Session == "" {
					t.Fatal("real native was not durably enrolled")
				}
				if err := c.store.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, episode.ID, native, driver) }); err != nil {
					return err
				}
				if mode == "node-restart-before-close" {
					machine.Close()
					machine, err = startOwnedRecoveryMachine(t, cfg)
					if err != nil {
						return err
					}
					transport.machine = machine
				}
				proof, err := manager.CloseRecoverySession(ctx, harness.Placement{Node: writer.Node, Harness: writer.Harness}, writer.Session, ws.Path)
				if err != nil {
					t.Fatalf("real exact process stop cannot be consumed after %s: state=%s stopped=%v err=%v", mode, proof.State, proof.ProcessStopped, err)
				}
				if !proof.ProcessStopped || proof.Command != nil {
					t.Fatalf("expected real whole-session proof with body pruned: %+v", proof)
				}
				if mode == "hub-refused-close-proof" {
					forceStopTrigger(t, c, `CREATE TRIGGER refuse_actual_native BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND EXISTS(SELECT 1 FROM json_each(NEW.data,'$.native_retirements') WHERE json_extract(value,'$.copy')=1 AND json_extract(value,'$.proof') IS NOT NULL) BEGIN SELECT RAISE(ABORT,'actual proof refused'); END`)
					if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: proof}); err == nil {
						t.Fatal("real proof rejection accepted")
					}
					refused, err := c.attempts.WorkspaceRecovery(ctx, episode.ID)
					if err != nil {
						return err
					}
					for _, n := range refused.NativeRetirements {
						if n.Copy && n.Proof != nil {
							t.Fatal("rejected proof became owner fact")
						}
					}
					machine.Close()
					machine, err = startOwnedRecoveryMachine(t, cfg)
					if err != nil {
						return err
					}
					transport.machine = machine
					if err := ledgerOf(t, c).Update(ctx, func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_actual_native"); return err }); err != nil {
						return err
					}
					repeated, err := manager.CloseRecoverySession(ctx, harness.Placement{Node: writer.Node, Harness: writer.Harness}, writer.Session, ws.Path)
					if err != nil {
						return err
					}
					if repeated.Sequence != proof.Sequence || repeated.Binding != proof.Binding || repeated.ContextID != proof.ContextID || !repeated.ProcessStopped || repeated.Command != nil {
						t.Fatalf("restart changed exact machine fact: %+v", repeated)
					}
					proof = repeated
				}
				if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: proof}); err != nil {
					return err
				}
				_, err = c.artifacts.CaptureRecoveryResidual(ctx, episode.ID, driver)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryProvenLostOpenConvergesItsExactNativeRetirement(t *testing.T) {
	for _, mode := range []string{"lost-reply", "retirement-before-proof", "refused-proof", "cancelled-open"} {
		t.Run(mode, func(t *testing.T) {
			c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
			p := project.Project{ID: "lost-open", Home: project.Home{Node: "node", Path: t.TempDir()}}
			if err := c.projects.Declare(t.Context(), []project.Project{p}); err != nil {
				t.Fatal(err)
			}
			p, found, err := c.projects.Get(t.Context(), p.ID)
			if err != nil || !found {
				t.Fatalf("declared project: %v %v", found, err)
			}
			if err := os.WriteFile(filepath.Join(p.Home.Path, "a"), []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			}
			base, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "before lost open")
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := tasks.Create(task.Task{Channel: "console:lost-open", Transport: "console", Member: "worker", ProjectID: p.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			original, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "lost-native-open", Kind: attempt.KindChat, TaskID: tracked.ID, TurnID: "web-lost-open", Execution: &token, Project: p.ID, Node: p.Home.Node, Harness: "mock", Agent: "worker", Base: base.ID, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()})
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.BindAttempt(token, original.ID, original.TurnID); err != nil {
				t.Fatal(err)
			}
			original, err = c.attempts.Advance(t.Context(), original.ID, attempt.Prepared, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			binding := nodewire.SessionBinding{ProjectID: p.ID, SessionID: attempt.RetainedSessionID(tracked.Channel, original.TaskID, original.Agent), TaskID: original.TaskID, AttemptID: original.ID, NodeID: original.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(original), TaskEpoch: original.Execution.Epoch}
			authority := &exactMachineAuthority{binding: binding}
			bin := filepath.Join(t.TempDir(), "mockagent")
			build := exec.Command("go", "build", "-o", bin, "./cmd/mockagent")
			build.Dir = "../.."
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("native build: %v %s", err, out)
			}
			cfg := node.ServerConfig{Name: original.Node, StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: authority}
			machine, err := startOwnedRecoveryMachine(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer machine.Close()
			req := nodewire.SessionRequest{Action: nodewire.SessionActionOpen, Authority: nodewire.SessionAuthority{ClusterID: "owned-cluster", CoordinatorNodeID: "owned-hub", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: binding, Harness: original.Harness, Workdir: p.Home.Path, CommandID: attempt.InputCommandID(original) + "/open"}
			var opened nodewire.SessionState
			if mode != "cancelled-open" {
				opened, err = machine.Do(t.Context(), "owned-cluster", req)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != "cancelled-open" && opened.ContextID == "" {
				t.Fatal("native opened without context")
			}
			if mode == "retirement-before-proof" {
				original, err = c.attempts.RecordSession(t.Context(), original.ID, "fixture", opened.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := c.attempts.MarkUnsettled(t.Context(), original.ID, "fixture", errors.New("open reply lost"), nil); err != nil {
				t.Fatal(err)
			}
			if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), original.ID, "owner", 0); err != nil {
				t.Fatal(err)
			}
			original, err = c.attempts.RecordForceStopResult(t.Context(), original.ID, 1, true, "stop_unproven")
			if err != nil {
				t.Fatal(err)
			}
			original, err = NewAbandonControl(c).AbandonAttempt(t.Context(), original.ID, "owner", 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), original.ID); err != nil {
				t.Fatal(err)
			}
			if mode == "retirement-before-proof" {
				if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), original.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
					r, err := c.attempts.EnrollRecoveryNatives(ctx, original.Abandoned.WorkspaceRecoveryID, false, driver)
					if err != nil {
						return err
					}
					if len(r.NativeRetirements) != 1 || r.NativeRetirements[0].Context != "" {
						t.Fatal("partial identity was not retained before stop proof")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			req.Action = nodewire.SessionActionKill
			if mode == "cancelled-open" {
				req.Action = nodewire.SessionActionCancelOpen
			}
			stopped, err := machine.Do(t.Context(), "owned-cluster", req)
			if err != nil {
				t.Fatal(err)
			}
			if !stopped.ProcessStopped || stopped.ContextID != opened.ContextID || stopped.OpenReceipt == nil {
				t.Fatalf("lost-open kill did not produce exact context proof: %+v", stopped)
			}
			bad := stopped
			bad.Binding.AttemptID = "another-native-binding"
			if _, err := c.attempts.ConfirmTaskStopped(t.Context(), original.ID, "fixture", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: bad}); err == nil {
				t.Fatal("new binding was accepted as old lost-open proof")
			}
			if mode == "refused-proof" {
				forceStopTrigger(t, c, `CREATE TRIGGER refuse_lost_proof BEFORE UPDATE ON operations WHEN NEW.id='lost-native-open' AND json_extract(NEW.data,'$.stop_evidence') IS NOT NULL BEGIN SELECT RAISE(ABORT,'lost proof refused'); END`)
				if _, err := c.attempts.ConfirmTaskStopped(t.Context(), original.ID, "fixture", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: stopped}); err == nil {
					t.Fatal("refused lost-open proof committed")
				}
				current, err := c.attempts.Get(t.Context(), original.ID)
				if err != nil || current.Session != "" || current.NativeContext != "" {
					t.Fatalf("refused proof changed identity: %+v %v", current, err)
				}
				if _, found, err := c.attempts.TaskStopReceipt(t.Context(), original.ID); err != nil || found {
					t.Fatalf("refused proof leaked owner receipt: %v %v", found, err)
				}
				machine.Close()
				machine, err = startOwnedRecoveryMachine(t, cfg)
				if err != nil {
					t.Fatal(err)
				}
				stopped, err = machine.Do(t.Context(), "owned-cluster", req)
				if err != nil {
					t.Fatal(err)
				}
				if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_lost_proof"); return err }); err != nil {
					t.Fatal(err)
				}
			}
			original, err = c.attempts.ConfirmTaskStopped(t.Context(), original.ID, "fixture", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: stopped})
			if err != nil {
				t.Fatal(err)
			}
			if original.NativeContext != stopped.ContextID || original.Session != stopped.ID {
				t.Fatalf("accepted stop lost exact native identity: session=%q context=%q want=%q", original.Session, original.NativeContext, stopped.ContextID)
			}
			again, err := attempt.New(ledgerOf(t, c)).ConfirmTaskStopped(t.Context(), original.ID, "fixture", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: stopped})
			if err != nil || again.Revision != original.Revision {
				t.Fatalf("reopened stop owner repeated the acceptance: %+v %v", again, err)
			}
			if err := c.attempts.CompleteAbandonDelivery(t.Context(), original, func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error) {
				return attempt.AbandonDeliveryNotRequired, nil
			}); err != nil {
				t.Fatal(err)
			}
			err = c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), original.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
				episode, err := c.attempts.EnrollRecoveryNatives(ctx, original.Abandoned.WorkspaceRecoveryID, false, driver)
				if err != nil {
					return err
				}
				if len(episode.NativeRetirements) != 1 {
					t.Fatalf("lost native enrollment: %+v", episode.NativeRetirements)
				}
				adopted, err := c.attempts.AdoptRecoveryNativeStop(ctx, episode.ID, episode.NativeRetirements[0], driver)
				if err != nil || !adopted {
					t.Fatalf("formally accepted real lost-open proof cannot retire: recordContext=%q machineContext=%q adopted=%v err=%v", original.NativeContext, stopped.ContextID, adopted, err)
				}
				native := episode.NativeRetirements[0]
				if native.Context != stopped.ContextID {
					t.Fatalf("retirement kept an unknown context after accepted proof: %+v", native)
				}
				if err := c.store.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, episode.ID, native, driver) }); err != nil {
					return err
				}
				if _, err := c.attempts.BeginRecoveryDrain(ctx, episode.ID, driver); err != nil {
					return err
				}
				_, err = c.artifacts.CaptureRecoveryResidual(ctx, episode.ID, driver)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}

		})
	}
}
