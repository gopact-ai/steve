package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// onboardingReceipt commits what an onboarding introduction leaves behind:
// a system task on the synthetic conversation, a chat attempt answered and
// accounted, and the node's settled input receipt. The introduction answers
// no channel message, so its turn carries no turn identity.
func onboardingReceipt(t *testing.T, book *ledger.Ledger) nodewire.SessionReceipt {
	t.Helper()
	ctx := t.Context()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	tracked, err := tasks.Create(task.Task{Channel: onboard.PendingID("owner"), Transport: "feishu", Requester: "owner",
		Member: "worker", ProjectID: "home", ChatType: "p2p", System: true, Goal: "introduction"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "node", task.TurnInput{ChatType: "p2p",
		Address: channel.Address{Channel: "feishu", Conversation: tracked.Channel}}); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := attempts.Open(ctx, attempt.Spec{ID: "att-onboarding", TaskID: tracked.ID, Kind: attempt.KindChat,
		Agent: "worker", Node: "node", Execution: &token, Scope: attempt.ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		record, err = attempts.Advance(ctx, record.ID, phase, "test", func(r *attempt.Record) {
			r.Session, r.NativeContext = "ns_"+strings.Repeat("a", 64), "native-context"
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := nodewire.NewSessionReceipt(nodewire.SessionState{ID: record.Session, ContextID: record.NativeContext,
		Binding: nodewire.SessionBinding{TaskID: tracked.ID, AttemptID: record.ID, NodeID: "node",
			SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, "worker"), TaskEpoch: token.Epoch,
			ExecutionEpoch: attempt.SessionExecutionEpoch(record)},
		Command: &nodewire.SessionCommand{ID: attempt.InputCommandID(record), InputSequence: 1, State: nodewire.SessionCommandCompleted, Settled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempts.MarkSessionSettled(ctx, record.ID, "test"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(turn.Result{Attempt: record.ID, AgentID: "worker", Text: "hello"})
	usage := &attempt.Usage{Reported: true, Input: 3, Output: 2}
	record, err = attempts.FinishCompletion(ctx, record.ID, "test", attempt.Completion{NodeReceipt: &receipt,
		Result: attempt.Result{Output: raw}, Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SettleAttempt(ctx, tracked.ID, record.ID, record.TurnID, record.EndedAt, task.OutcomeOK,
		task.RecoveryUsage{Reported: true, Tokens: task.Tokens{Input: 3, Output: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(tracked.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// A hub starting after an onboarding introduction finds its settled receipt
// pending. The introduction was sent straight to the owner, with no durable
// delivery any owner could confirm, so the receipt is kept as pending work:
// not an error, and never acknowledged to the node.
func TestNodeReceiptReconcilerKeepsOnboardingReceiptWithoutError(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	receipt := onboardingReceipt(t, book)
	attempts := attempt.New(book)
	acked := 0
	reconciler := &nodeReceiptReconciler{book: book, attempts: attempts,
		nodes: receiptAckFunc(func(context.Context, string, nodewire.SessionReceiptRequest) error { acked++; return nil })}
	if err := reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("onboarding receipt reported as a failure: %v", err)
	}
	pending, err := attempts.PendingNodeReceipts(t.Context(), "", 128)
	if err != nil || len(pending) != 1 || pending[0] != receipt || acked != 0 {
		t.Fatalf("onboarding receipt must stay pending and unacknowledged: %+v acked=%d %v", pending, acked, err)
	}
}
