package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

type receiptChatHandler struct {
	turntest.IdleCoordinator
	answer func(context.Context, turn.Request) (turn.Result, error)
}

func (h receiptChatHandler) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	return h.answer(ctx, req)
}

type receiptAckFunc func(context.Context, string, nodewire.SessionReceiptRequest) error

func (f receiptAckFunc) AcknowledgeNodeReceipt(ctx context.Context, node string, req nodewire.SessionReceiptRequest) error {
	return f(ctx, node, req)
}

func runReceiptNode(t *testing.T, ctx context.Context, cfg node.ServerConfig) (*node.Server, func()) {
	t.Helper()
	server := node.NewServer(cfg)
	nodeCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(nodeCtx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-served }) }
	t.Cleanup(stop)
	for server.Addr() == "" || strings.HasSuffix(server.Addr(), ":0") {
		select {
		case <-ctx.Done():
			t.Fatal("isolated receipt node did not start")
		case <-time.After(time.Millisecond):
		}
	}
	return server, stop
}

func TestNodeReceiptsConsoleClosesAuthenticatedNativeInput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated mock ACP: %v %s", err, out)
	}
	for _, key := range []string{"client-key", ""} {
		t.Run("key="+key, func(t *testing.T) { testNodeReceiptConsoleClosure(t, bin, key) })
	}
}

func testNodeReceiptConsoleClosure(t *testing.T, bin, key string) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	cfg := node.ServerConfig{Name: "worker", Listen: "127.0.0.1:0", Token: "receipt-test", StateDir: dir,
		WorkspaceRoot: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: node.CoordinatorSessionAuthorizer{}}
	server, stopNode := runReceiptNode(t, ctx, cfg)
	hubDir := t.TempDir()
	book, err := ledger.Open(hubDir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { book.Close() }()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	registry := node.NewRegistry("receipt-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "receipt-test"}})
	defer func() { registry.Close() }()
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	manager.SetTransports(registry)
	authority := nodewire.SessionAuthority{ClusterID: "receipt-cluster", CoordinatorNodeID: "hub", CoordinatorEpoch: 1, WriterGeneration: 1}
	registry.SetSessionAuthorizer(func(_ context.Context, node string, a nodewire.SessionAuthority, _ nodewire.SessionBinding, _ nodewire.SessionAction) error {
		if node != "worker" || a != authority {
			return errors.New("wrong test activation")
		}
		return nil
	})
	var ackCalls atomic.Int32
	authorizeReceipt := func(ctx context.Context, node string, a nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
		if node != "worker" || a != authority {
			return errors.New("wrong authenticated node or activation")
		}
		ackCalls.Add(1)
		return cluster.ReadNodeReceiptProof(ctx, book, receipt)
	}
	registry.SetNodeReceiptAuthorizer(authorizeReceipt)
	reconciler := &nodeReceiptReconciler{book: book, attempts: attempts, nodes: registry, authority: authority}
	var record attempt.Record
	var bound harness.NodeSessionContext
	workdir := t.TempDir()
	cons := console.New(receiptChatHandler{answer: func(ctx context.Context, req turn.Request) (turn.Result, error) {
		tracked, err := tasks.Create(task.Task{Channel: req.ConversationID, Transport: req.Channel, Requester: req.SenderOpenID,
			Member: "mock", ProjectID: "p", Goal: "receipt closure"})
		if err != nil {
			return turn.Result{}, err
		}
		if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
			return turn.Result{}, err
		}
		token, err := tasks.ExecutionToken(tracked.ID)
		if err != nil {
			return turn.Result{}, err
		}
		var result turn.Result
		finished, err := lifecycle.Run(ctx, lifecycle.Options{
			Attempts: attempts, Sessions: manager, Actor: "test",
			Spec: attempt.Spec{ID: "native-receipt", TaskID: tracked.ID, TurnID: req.MessageID, Kind: attempt.KindChat,
				Agent: "mock", Node: "worker", Harness: "mock", Project: "p", Execution: &token, Scope: attempt.ScopePathSet,
				Workspace: project.Workspace{ID: "work", Project: "p", Node: "worker", Kind: project.KindWorktree, Path: workdir}},
			At: harness.Placement{Node: "worker", Harness: "mock"}, Workdir: workdir, Prompt: req.Input, TurnPrompt: true,
			Leased: func(ctx context.Context, e *lifecycle.Execution) (context.Context, error) {
				if err := tasks.BindAttempt(token, e.Record.ID, req.MessageID); err != nil {
					return ctx, err
				}
				bound = harness.NodeSessionContext{Authority: authority, CommandID: req.MessageID, Binding: nodewire.SessionBinding{
					ProjectID: "p", SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, "mock"), TaskID: tracked.ID,
					AttemptID: e.Record.ID, NodeID: "worker", ExecutionEpoch: attempt.SessionExecutionEpoch(e.Record), TaskEpoch: token.Epoch}}
				return harness.WithNodeSession(ctx, bound), nil
			},
			Finish: func(_ context.Context, e *lifecycle.Execution) (attempt.Completion, error) {
				result = turn.Result{Attempt: e.Record.ID, AgentID: "mock", Text: e.Outcome.Answer}
				raw, err := json.Marshal(result)
				return attempt.Completion{Result: attempt.Result{Output: raw}, Usage: e.Usage}, err
			},
			Settlement: lifecycle.Settlement{KeepSession: true, DetachManaged: true, RejectManaged: true},
		})
		if err != nil {
			return result, err
		}
		record = finished.Record
		if record.NodeReceipt == nil {
			return result, errors.New("actual authenticated native terminal receipt did not reach completion")
		}
		if err := reconciler.Reconcile(ctx); err != nil {
			return result, err
		}
		if ackCalls.Load() != 0 {
			return result, errors.New("missing task accounting nevertheless sent ack RPC")
		}
		u := record.Usage
		if err := tasks.SettleAttempt(tracked.ID, record.ID, record.TurnID, record.EndedAt, task.OutcomeOK,
			task.RecoveryUsage{Reported: u.Reported, Model: u.Model, Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite}}); err != nil {
			return result, err
		}
		// No reply has been committed by the console yet.
		reconciler.after = ""
		if err := reconciler.Reconcile(ctx); err != nil {
			return result, err
		}
		if ackCalls.Load() != 0 {
			return result, errors.New("missing durable delivery nevertheless sent ack RPC")
		}
		return result, nil
	}}, "owner", nil)
	if err := cons.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	reply, err := cons.SendCommand(ctx, "native", "reportusage", key)
	if err != nil {
		t.Fatalf("real Console → lifecycle → ACP failed: %v", err)
	}
	if reply.AttemptID != record.ID || record.NodeReceipt == nil {
		t.Fatal("real delivery lost its server attempt/receipt")
	}
	// Rebind the live native conversation and accept a new input before the
	// original ack arrives. Its active/unknown evidence must remain untouched.
	late := nodewire.SessionRequest{Action: nodewire.SessionActionOpen, Authority: authority, Binding: bound.Binding,
		ID: record.Session, Harness: "mock", Workdir: workdir, Permission: permission.PolicyRead, CommandID: "late/open"}
	late.Binding.AttemptID, late.Binding.TaskID = "late-attempt", "late-task"
	if state, err := registry.NodeSession(ctx, "worker", late); err != nil || state.ID != record.Session {
		t.Fatalf("warm rebind: %+v %v", state, err)
	}
	late.Action, late.CommandID = nodewire.SessionActionAttach, "late-input"
	hint, err := registry.NodeSession(ctx, "worker", late)
	if err != nil || hint.Binding != late.Binding || hint.NextInputSequence != record.NodeReceipt.InputSequence+1 {
		t.Fatalf("late binding hint: %+v %v", hint, err)
	}
	late.Action, late.InputSequence, late.Text = nodewire.SessionActionPrompt, hint.NextInputSequence, "slow"
	if _, err := registry.NodeSession(ctx, "worker", late); err != nil {
		t.Fatal(err)
	}

	// Lose only the response after the real RPC has deleted the original rows.
	// The runtime must retain pending and retry the same receipt after restart.
	lost := errors.New("injected lost ack response")
	reconciler.nodes = receiptAckFunc(func(ctx context.Context, node string, req nodewire.SessionReceiptRequest) error {
		if err := registry.AcknowledgeNodeReceipt(ctx, node, req); err != nil {
			return err
		}
		return lost
	})
	if err := reconciler.Reconcile(ctx); !errors.Is(err, lost) {
		t.Fatalf("lost response did not leave a retry: %v", err)
	}
	pending, err := attempts.PendingNodeReceipts(ctx, "", 128)
	if err != nil || len(pending) != 1 || pending[0] != *record.NodeReceipt || ackCalls.Load() != 1 {
		t.Fatalf("lost response consumed original pending: %+v calls=%d %v", pending, ackCalls.Load(), err)
	}
	stopNode()
	registry.Close()
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(hubDir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	attempts = attempt.New(book)
	server, _ = runReceiptNode(t, ctx, cfg)
	registry = node.NewRegistry("receipt-cluster", map[string]node.Config{"worker": {Addr: server.Addr(), Token: "receipt-test"}})
	registry.SetSessionAuthorizer(func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error {
		return nil
	})
	registry.SetNodeReceiptAuthorizer(authorizeReceipt)
	reconciler = &nodeReceiptReconciler{book: book, attempts: attempts, nodes: registry, authority: authority}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_ack_index BEFORE DELETE ON bindings
		WHEN OLD.kind='attempt-node-receipt-pending' BEGIN SELECT RAISE(ABORT,'pending unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err == nil || !strings.Contains(err.Error(), "pending unavailable") {
		t.Fatalf("failed hub acknowledgement commit was hidden: %v", err)
	}
	pending, err = attempts.PendingNodeReceipts(ctx, "", 128)
	if err != nil || len(pending) != 1 || ackCalls.Load() != 2 {
		t.Fatalf("failed pending commit lost retry: %+v %v", pending, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_ack_index`); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = attempts.PendingNodeReceipts(ctx, "", 128)
	if err != nil || len(pending) != 0 || ackCalls.Load() != 3 {
		t.Fatalf("actual ack retry did not consume durable pending: %v calls=%d pending=%+v", err, ackCalls.Load(), pending)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "node-sessions", "sessions.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var commands, questions int
	if err := db.QueryRow(`SELECT command_count,question_count FROM sessions WHERE id=?`, record.Session).Scan(&commands, &questions); err != nil || commands != 1 || questions != 0 {
		t.Fatalf("late ack failed to preserve only the later input: %d/%d %v", commands, questions, err)
	}
	var retained string
	if err := db.QueryRow(`SELECT id FROM session_commands WHERE session_id=?`, record.Session).Scan(&retained); err != nil || retained != late.CommandID {
		t.Fatalf("late ack retained wrong command: %s %v", retained, err)
	}
	if err := registry.AcknowledgeNodeReceipt(ctx, "worker", nodewire.SessionReceiptRequest{Authority: authority, Receipt: *record.NodeReceipt}); err != nil {
		t.Fatalf("lost-response retry did not verify its exact durable receipt: %v", err)
	}
	for _, sequence := range []uint64{0, record.NodeReceipt.InputSequence} {
		_, err := registry.NodeSession(ctx, "worker", nodewire.SessionRequest{Action: nodewire.SessionActionPrompt, Authority: authority,
			Binding: bound.Binding, ID: record.Session, CommandID: bound.CommandID, InputSequence: sequence, Text: "must not run"})
		if err == nil {
			t.Fatal("ack deletion permitted old input replay")
		}
	}
	durable, err := attempts.Get(ctx, record.ID)
	if err != nil || durable.NodeReceipt == nil || durable.Result == nil {
		t.Fatal("node ack deleted hub terminal evidence")
	}
}
