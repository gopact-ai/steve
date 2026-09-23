package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

type nodeReceiptHandler func(turn.Request) (turn.Result, error)

func (h nodeReceiptHandler) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	return h(req)
}

func TestNodeReceiptProofNeedsExactResultAccountingAndDelivery(t *testing.T) {
	for _, missing := range []string{"", "accounting", "unknown-usage", "delivery", "wrong-sequence", "wrong-digest", "other-transport"} {
		t.Run(missing, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			receipt, reply := committedConsoleReceipt(t, book, "node", missing)
			switch missing {
			case "delivery":
				if _, err := book.DB().Exec(`DELETE FROM bindings WHERE kind='console-exchange' AND id=?`, reply.ExchangeID); err != nil {
					t.Fatal(err)
				}
			case "wrong-sequence":
				receipt.InputSequence++
			case "wrong-digest":
				receipt.Digest = strings.Repeat("b", 64)
			}
			err = ReadNodeReceiptProof(t.Context(), book, receipt)
			if (err == nil) != (missing == "") {
				t.Fatalf("proof accepted/refused incorrectly (%s): %v", missing, err)
			}
		})
	}
}

func TestNodeReceiptProofRejectsAccountingIdentitySubstitution(t *testing.T) {
	for _, field := range []string{"node", "member", "independent", "execution_epoch", "model", "tokens.input", "usage_known"} {
		t.Run(field, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			receipt, _ := committedConsoleReceipt(t, book, "node", "")
			if err := ReadNodeReceiptProof(t.Context(), book, receipt); err != nil {
				t.Fatalf("control lacks authentic proof: %v", err)
			}
			var value any = "unrelated-owner"
			switch field {
			case "execution_epoch", "tokens.input":
				value = int64(12345)
			case "usage_known":
				value = false
			case "independent":
				value = true
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := book.DB().Exec(`UPDATE bindings SET data=json_set(data,?,json(?))
				WHERE kind='task-attempt'`, "$."+field, string(raw)); err != nil {
				t.Fatal(err)
			}
			if err := ReadNodeReceiptProof(t.Context(), book, receipt); !errors.Is(err, ErrNodeReceiptPending) {
				t.Fatalf("substituted accounting %s must not authorize deletion: %v", field, err)
			}
		})
	}
}

// A chat that ended without an answer is closed Failed, and its accounting
// records how it ended: a failure, a cancellation or a timeout. Each is a
// settled ending whose receipt can be released.
func TestNodeReceiptProofAcceptsEveryFailedEnding(t *testing.T) {
	for _, ending := range []task.Outcome{task.OutcomeError, task.OutcomeCancelled, task.OutcomeTimeout, task.OutcomeOK} {
		t.Run(string(ending), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			receipt, _ := failedConsoleReceipt(t, book, ending)
			err = ReadNodeReceiptProof(t.Context(), book, receipt)
			if ending == task.OutcomeOK {
				// A failed attempt accounted as a success is not its own.
				if !errors.Is(err, ErrNodeReceiptPending) {
					t.Fatalf("failed attempt accounted ok authorized deletion: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("failed attempt ending %s kept its receipt: %v", ending, err)
			}
		})
	}
}

func committedConsoleReceipt(t *testing.T, book *ledger.Ledger, node, missing string) (nodewire.SessionReceipt, consoleapi.Reply) {
	t.Helper()
	return consoleReceipt(t, book, node, missing, "")
}

// failedConsoleReceipt closes the chat Failed and accounts it as ending.
func failedConsoleReceipt(t *testing.T, book *ledger.Ledger, ending task.Outcome) (nodewire.SessionReceipt, consoleapi.Reply) {
	t.Helper()
	return consoleReceipt(t, book, "node", "", ending)
}

// consoleReceipt runs one console chat to a terminal attempt with a node
// receipt: Bound and accounted ok, or, when failed is set, Failed and
// accounted as failed.
func consoleReceipt(t *testing.T, book *ledger.Ledger, node, missing string, failed task.Outcome) (nodewire.SessionReceipt, consoleapi.Reply) {
	t.Helper()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	var receipt nodewire.SessionReceipt
	run := nodeReceiptHandler(func(req turn.Request) (turn.Result, error) {
		transport := "console"
		if missing == "other-transport" {
			transport = "unsupported"
		}
		tracked, err := tasks.Create(task.Task{Channel: req.ConversationID, Transport: transport, Member: "worker", Requester: "owner"})
		if err != nil {
			return turn.Result{}, err
		}
		if _, err = tasks.Begin(tracked.ID, "worker", node, ""); err != nil {
			return turn.Result{}, err
		}
		token, err := tasks.ExecutionToken(tracked.ID)
		if err != nil {
			return turn.Result{}, err
		}
		record, err := attempts.Open(t.Context(), attempt.Spec{ID: "receipt-attempt", TaskID: tracked.ID, TurnID: req.MessageID,
			Kind: attempt.KindChat, Agent: "worker", Node: node, Execution: &token, Scope: attempt.ScopePathSet})
		if err != nil {
			return turn.Result{}, err
		}
		if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
			return turn.Result{}, err
		}
		for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
			record, err = attempts.Advance(t.Context(), record.ID, phase, "test", func(r *attempt.Record) {
				r.Session, r.NativeContext = "ns_"+strings.Repeat("a", 64), "native-context"
			})
			if err != nil {
				return turn.Result{}, err
			}
		}
		receipt, err = nodewire.NewSessionReceipt(nodewire.SessionState{ID: record.Session, ContextID: record.NativeContext,
			Binding: nodewire.SessionBinding{TaskID: tracked.ID, AttemptID: record.ID, NodeID: node, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, "worker"),
				TaskEpoch: token.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(record)},
			Command: &nodewire.SessionCommand{ID: record.TurnID, InputSequence: 1, State: nodewire.SessionCommandCompleted, Settled: true}})
		if err != nil {
			return turn.Result{}, err
		}
		if err := attempts.MarkSessionSettled(t.Context(), record.ID, "test"); err != nil {
			return turn.Result{}, err
		}
		result := turn.Result{Attempt: record.ID, AgentID: "worker", Text: "answer"}
		raw, _ := json.Marshal(result)
		usage := &attempt.Usage{Reported: missing != "unknown-usage"}
		if failed != "" {
			record, err = attempts.Advance(t.Context(), record.ID, attempt.Failed, "test", func(r *attempt.Record) {
				r.Error, r.Usage, r.NodeReceipt = "turn ended", usage, &receipt
			})
			if err != nil {
				return turn.Result{}, err
			}
			if err := tasks.SettleAttempt(tracked.ID, record.ID, record.TurnID, record.EndedAt, failed, task.RecoveryUsage{Reported: usage.Reported}); err != nil {
				return turn.Result{}, err
			}
			return turn.Result{Attempt: record.ID}, errors.New("turn ended")
		}
		record, err = attempts.FinishCompletion(t.Context(), record.ID, "test", attempt.Completion{
			NodeReceipt: &receipt, Result: attempt.Result{Output: raw}, Usage: usage})
		if err != nil {
			return turn.Result{}, err
		}
		if missing != "accounting" {
			if err := tasks.SettleAttempt(tracked.ID, record.ID, record.TurnID, record.EndedAt, task.OutcomeOK, task.RecoveryUsage{Reported: usage.Reported}); err != nil {
				return turn.Result{}, err
			}
		}
		// Exact result/accounting exist here, but Send has not yet
		// committed its reply. It must not authorize premature deletion.
		if err := ReadNodeReceiptProof(t.Context(), book, receipt); err == nil {
			return turn.Result{}, errors.New("receipt authorized before delivery commit")
		}
		return result, nil
	})
	cons := console.New(run, "owner", nil)
	if err := cons.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	reply, err := cons.Send(t.Context(), "proof", "work")
	if err != nil && (failed == "" || reply.Error == "") {
		t.Fatal(err)
	}
	return receipt, reply
}

func TestPeerNodeReceiptAuthorityRequiresExactActivationAndOriginalProof(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	options.Activate = testPeerApplication(t, new(atomic.Int32))
	peer := StartTestPeer(t, options)
	active := WaitPeerReady(t, peer)
	receipt, _ := committedConsoleReceipt(t, active.Ledger, peer.Config.NodeID, "")
	authority := nodewire.SessionAuthority{ClusterID: peer.Config.ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}
	if err := peer.AuthorizeNodeReceipt(t.Context(), active.NodeID, authority, receipt); err != nil {
		t.Fatal(err)
	}
	authorize := peer.ApplicationReceiptAuthorizer(active)
	if err := authorize(t.Context(), peer.Config.NodeID, authority, receipt); err != nil {
		t.Fatal(err)
	}
	if err := peer.AuthorizeNodeReceipt(t.Context(), "imposter", authority, receipt); err == nil {
		t.Fatal("unrelated authenticated peer authorized deletion")
	}
	for _, field := range []string{"cluster", "epoch", "writer", "node", "binding", "digest"} {
		changed, claim := authority, receipt
		node := peer.Config.NodeID
		switch field {
		case "cluster":
			changed.ClusterID = "other"
		case "epoch":
			changed.CoordinatorEpoch++
		case "writer":
			changed.WriterGeneration++
		case "node":
			node = "other"
		case "binding":
			claim.Binding.AttemptID = "other"
		case "digest":
			claim.Digest = strings.Repeat("b", 64)
		}
		if err := authorize(t.Context(), node, changed, claim); err == nil {
			t.Fatalf("%s mismatch authorized deletion", field)
		}
	}
	// Revoking execution must not revoke cleanup of an already committed input.
	tasks, err := task.OpenLedger(active.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(receipt.Binding.TaskID, "worker", peer.Config.NodeID, ""); err != nil {
		t.Fatal(err)
	}
	if err := authorize(t.Context(), peer.Config.NodeID, authority, receipt); err != nil {
		t.Fatalf("new task execution hid original receipt: %v", err)
	}
	cancelled, cancel := context.WithCancel(active.Context)
	cancel()
	active.Context = cancelled
	if err := peer.ApplicationReceiptAuthorizer(active)(t.Context(), peer.Config.NodeID, authority, receipt); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended activation: %v", err)
	}
	stale := authority
	stale.WriterGeneration++
	if err := peer.AuthorizeNodeReceipt(t.Context(), active.NodeID, stale, receipt); !errors.Is(err, coordination.ErrStaleEpoch) {
		t.Fatalf("stale full peer writer: %v", err)
	}
}
