package node

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type receiptAuthority struct {
	sessionAuthorityTest
	want  nodewire.SessionReceipt
	calls int
	deny  bool
}

func (a *receiptAuthority) AuthorizeNodeReceipt(_ context.Context, principal string, _ nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	a.calls++
	if a.deny || principal != "cluster-1" || receipt != a.want {
		return errors.New("no exact durable result/accounting/delivery proof")
	}
	return nil
}

func ackFixture(t *testing.T) (*ownedSession, nodewire.SessionReceiptRequest, *receiptAuthority) {
	t.Helper()
	one := progressSession(t)
	request := nodeSessionRequest("attach")
	next := one.copyLocked()
	next.ClusterID, next.Authority, next.UpstreamID = request.Authority.ClusterID, request.Authority, "native-upstream"
	next.State.State, next.State.ContextID = nodewire.SessionIdle, "native-context"
	next.State.Questions = []nodewire.SessionQuestion{{ID: "question", CommandID: next.CurrentCommand, State: nodewire.SessionQuestionInterrupted}}
	command := next.Commands[next.CurrentCommand]
	command.State, command.Settled, command.Output = nodewire.SessionCommandCompleted, true, "answer"
	next.Commands[next.CurrentCommand] = command
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	receipt := one.record.Commands[one.record.CurrentCommand].Receipt
	authority := &receiptAuthority{want: receipt}
	cfg := one.service.server.conf()
	cfg.SessionAuthorizer = authority
	one.service.server.cfg.Store(&cfg)
	one.service.sessions = map[string]*ownedSession{one.record.State.ID: one}
	return one, nodewire.SessionReceiptRequest{Authority: request.Authority, Receipt: receipt}, authority
}

func TestNodeReceiptAckDeletesOnlyExactOriginalInputAndPreservesHighwater(t *testing.T) {
	one, request, authority := ackFixture(t)
	store, _ := one.service.recordsStore()
	var beforeBytes int
	_ = store.db.QueryRow(`SELECT retained_bytes FROM sessions WHERE id=?`, request.Receipt.SessionID).Scan(&beforeBytes)
	// Another binding starts later on this same native conversation. Ack must
	// not consult only that binding's terminal status or remove its evidence.
	next := one.copyLocked()
	next.State.Binding.AttemptID = "next-attempt"
	next.BindingInputStart = next.State.InputAccepted
	next.State.InputAccepted++
	next.CurrentCommand = "next-input"
	next.Commands[next.CurrentCommand] = nodewire.SessionCommand{ID: next.CurrentCommand, InputSequence: 2, State: nodewire.SessionCommandRunning}
	next.CommandHashes[next.CurrentCommand] = "next-hash"
	next.State.Questions = nil
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", request); err != nil {
		t.Fatal(err)
	}
	current := one.record
	if current.State.InputAccepted != 2 || current.BindingInputStart != 1 || current.UpstreamID != "native-upstream" ||
		current.State.ContextID != request.Receipt.ContextID || !one.runningLocked() {
		t.Fatal("ack reset native identity/highwater or the new binding's active input")
	}
	var commands, questions, afterBytes int
	if err := store.db.QueryRow(`SELECT command_count,question_count,retained_bytes FROM sessions WHERE id=?`, current.State.ID).
		Scan(&commands, &questions, &afterBytes); err != nil {
		t.Fatal(err)
	}
	if commands != 1 || questions != 0 || afterBytes >= beforeBytes {
		t.Fatalf("ack did not free exact retained quota: %d/%d %d >= %d", commands, questions, afterBytes, beforeBytes)
	}
	if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", request); err != nil || authority.calls != 2 {
		t.Fatalf("exact retry must reauthorize even after deletion: %v calls=%d", err, authority.calls)
	}
	authority.deny = true
	if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", request); err == nil {
		t.Fatal("absent row bypassed durable hub proof")
	}
	req := nodeSessionRequest(nodewire.SessionActionPrompt)
	req.ID, req.CommandID, req.InputSequence = current.State.ID, request.Receipt.CommandID, request.Receipt.InputSequence
	if _, err := one.prompt(req); err == nil {
		t.Fatal("ack made an old sequence executable again")
	}
}

func TestNodeReceiptAckFailsClosedAndTransactionFailurePublishesNothing(t *testing.T) {
	for _, mode := range []string{"denied", "wrong-digest", "wrong-binding", "wrong-sequence", "pending-question", "sqlite-failure"} {
		t.Run(mode, func(t *testing.T) {
			one, req, authority := ackFixture(t)
			store, _ := one.service.recordsStore()
			switch mode {
			case "denied":
				authority.deny = true
			case "wrong-digest":
				req.Receipt.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				authority.want = req.Receipt
			case "wrong-binding":
				req.Receipt.Binding.AttemptID = "other"
				authority.want = req.Receipt
			case "wrong-sequence":
				req.Receipt.InputSequence++
				authority.want = req.Receipt
			case "pending-question":
				_, _ = store.db.Exec(`UPDATE session_questions SET settled=0`)
			case "sqlite-failure":
				if _, err := store.db.Exec(`CREATE TRIGGER deny_ack BEFORE DELETE ON session_commands BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			before, changed := one.copyLocked(), one.changed
			if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", req); err == nil {
				t.Fatal("unproven or failed acknowledgement succeeded")
			}
			var commands, questions int
			_ = store.db.QueryRow(`SELECT command_count,question_count FROM sessions WHERE id=?`, one.record.State.ID).Scan(&commands, &questions)
			if commands != 1 || questions != 1 || !reflect.DeepEqual(before, one.copyLocked()) || changed != one.changed {
				t.Fatal("refused/failed ack changed durable rows, memory or events")
			}
			select {
			case <-changed:
				t.Fatal("failed ack published a change")
			default:
			}
		})
	}
}
