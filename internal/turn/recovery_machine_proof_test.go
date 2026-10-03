package turn

import (
	"context"
	"errors"
	"net"
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
	"github.com/gopact-ai/steve/internal/state"
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
