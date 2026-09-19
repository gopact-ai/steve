package attempt

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func nodeReceiptCompletion(t *testing.T) (*Service, Record, Completion) {
	t.Helper()
	service, _, record, evidence, _ := retainedFixture(t)
	evidence.Session.ContextID = "original-context"
	evidence.Session.Command.State = nodewire.SessionCommandCompleted
	evidence.Session.Command.Settled = true
	receipt, err := nodewire.NewSessionReceipt(evidence.Session)
	if err != nil {
		t.Fatal(err)
	}
	record, err = service.Advance(t.Context(), record.ID, BindReady, "test", func(r *Record) {
		r.NativeContext = evidence.Session.ContextID
		settled := true
		r.SessionSettled = &settled
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, record, Completion{NodeReceipt: &receipt, Result: Result{Summary: "answer", Artifact: "artifact"},
		Usage: &Usage{Reported: true, Input: 5}}
}

func TestNodeReceiptAndPendingCommitAtomicallyWithResult(t *testing.T) {
	service, record, completion := nodeReceiptCompletion(t)
	completed, err := service.Complete(t.Context(), record.ID, "test", completion)
	if err != nil || completed.NodeReceipt == nil || *completed.NodeReceipt != *completion.NodeReceipt {
		t.Fatalf("completion lost node evidence: %+v %v", completed, err)
	}
	pending, err := service.PendingNodeReceipts(t.Context(), "", 128)
	if err != nil || len(pending) != 1 || pending[0] != *completion.NodeReceipt {
		t.Fatalf("pending index is not atomic with result: %+v %v", pending, err)
	}
	other := *completion.NodeReceipt
	other.InputSequence++
	if err := service.AcknowledgeNodeReceipt(t.Context(), other); err == nil {
		t.Fatal("unrelated node response removed pending receipt")
	}
	if err := service.AcknowledgeNodeReceipt(t.Context(), *completion.NodeReceipt); err != nil {
		t.Fatal(err)
	}
	pending, err = service.PendingNodeReceipts(t.Context(), "", 128)
	if err != nil || len(pending) != 0 {
		t.Fatalf("ack did not consume pending index: %+v %v", pending, err)
	}
	durable, err := service.Get(t.Context(), record.ID)
	if err != nil || durable.Result == nil || durable.NodeReceipt == nil || *durable.NodeReceipt != *completion.NodeReceipt {
		t.Fatal("ack removed durable terminal result or original receipt identity")
	}
}

func TestNodeReceiptCASFailureAndWrongBindingCannotCreatePending(t *testing.T) {
	for _, mode := range []string{"cas-conflict", "wrong-command", "wrong-binding"} {
		t.Run(mode, func(t *testing.T) {
			service, record, completion := nodeReceiptCompletion(t)
			switch mode {
			case "cas-conflict":
				if err := service.l.Update(t.Context(), func(tx *ledger.Tx) error {
					_, err := tx.CompareAndSetName("result", 0, "winner")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				completion.Binding = &NameBinding{Name: "result"}
			case "wrong-command":
				completion.NodeReceipt.CommandID = "other-input"
			case "wrong-binding":
				completion.NodeReceipt.Binding.TaskEpoch++
			}
			if _, err := service.Complete(t.Context(), record.ID, "test", completion); err == nil {
				t.Fatal("invalid completion was accepted")
			}
			got, _ := service.Get(t.Context(), record.ID)
			pending, err := service.PendingNodeReceipts(t.Context(), "", 128)
			if err != nil || len(pending) != 0 || got.State != BindReady || got.NodeReceipt != nil {
				t.Fatalf("partial terminal receipt committed: %+v %+v %v", got, pending, err)
			}
		})
	}
}

func TestNodeReceiptPendingQueryDoesNotDecodeClosedHistory(t *testing.T) {
	service, record, completion := nodeReceiptCompletion(t)
	if _, err := service.Complete(t.Context(), record.ID, "test", completion); err != nil {
		t.Fatal(err)
	}
	if err := service.l.PutBinding(t.Context(), "unrelated", "bad", json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	got, err := service.PendingNodeReceipts(t.Context(), "", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("pending lookup touched unrelated history: %+v %v", got, err)
	}
	if err := service.l.PutBinding(t.Context(), nodeReceiptPendingKind, "corrupt", json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PendingNodeReceipts(t.Context(), "", 128); err == nil || errors.Is(err, ledger.ErrConflict) {
		t.Fatal("corrupt pending evidence was skipped")
	}
}
