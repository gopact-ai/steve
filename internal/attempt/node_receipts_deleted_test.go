package attempt

import (
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

// completedReceiptOfDeletedTask commits a terminal receipt and then deletes
// the task's conversation the way the owner does: the row is closed first,
// and the task goes with its header and accounting.
func completedReceiptOfDeletedTask(t *testing.T) (*Service, Completion) {
	t.Helper()
	service, record, completion := nodeReceiptCompletion(t)
	if _, err := service.Complete(t.Context(), record.ID, "test", completion); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(service.l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish(record.TaskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.DeleteChannel("console:main"); err != nil {
		t.Fatal(err)
	}
	return service, completion
}

// A receipt whose task was deleted with its conversation can never be
// proved against that conversation again. The node still holds the
// session evidence until the coordinator acknowledges it, so the exact
// terminal receipt of a task that is gone is acknowledged, not retried on
// every pass forever.
func TestNodeReceiptOfADeletedTaskIsAcknowledged(t *testing.T) {
	service, completion := completedReceiptOfDeletedTask(t)
	if err := service.AcknowledgeNodeReceipt(t.Context(), *completion.NodeReceipt); err != nil {
		t.Fatalf("receipt of a deleted task stays pending: %v", err)
	}
	pending, err := service.PendingNodeReceipts(t.Context(), "", 128)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v; want the receipt consumed", pending, err)
	}
}

// Only a task that is wholly gone releases its receipts. A task that still
// exists is checked against its conversation as before, and one whose
// header is missing while its accounting remains is not a deletion.
func TestNodeReceiptOfAnExistingTaskIsStillCheckedAgainstItsConversation(t *testing.T) {
	for _, mode := range []string{"moved-conversation", "accounting-left"} {
		t.Run(mode, func(t *testing.T) {
			service, record, completion := nodeReceiptCompletion(t)
			if _, err := service.Complete(t.Context(), record.ID, "test", completion); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "moved-conversation":
				if _, err := service.l.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.channel','console:other') WHERE kind='task' AND id=?`, record.TaskID); err != nil {
					t.Fatal(err)
				}
			case "accounting-left":
				if _, err := service.l.DB().Exec(`DELETE FROM bindings WHERE kind='task' AND id=?`, record.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.AcknowledgeNodeReceipt(t.Context(), *completion.NodeReceipt); err == nil {
				t.Fatal("receipt acknowledged without its conversation")
			}
			pending, err := service.PendingNodeReceipts(t.Context(), "", 128)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending = %+v, %v; want the receipt kept", pending, err)
			}
		})
	}
}
