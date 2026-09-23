package console

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestManualResumeWakeCannotReleaseStartupAccountingBarrier(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	owner, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	row, err := owner.Create(task.Task{Transport: "console", Channel: "console:manual", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.SetAside(row.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	row, _ = owner.Get(row.ID)
	a := task.ResumeAdmission{ID: "resume-manual", TaskID: row.ID, Epoch: row.ExecutionEpoch + 1}
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueTaskResume(t.Context(), "startup", "parent", "startup-key", "worker", "startup", "startup prompt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(t.Context(), row.Channel, row.ID, row.Member, "manual", "continue", a, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Resume(row.ID, row.ExecutionEpoch, row.State, a); err != nil {
		t.Fatal(err)
	}
	if err := s.DispatchResume(row.Channel, a); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.ConversationID != row.Channel || call.req.ResumeAdmission != a {
		t.Fatalf("manual wake dispatched another conversation: %+v", call.req)
	}
	call.finish <- nil
	awaitExchange(t, s, s.Queue(row.Channel)[0].ID)
	state, err := LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	if e := state.Exchanges["console:startup"][0]; e.State != "queued" || !e.RecoveryPending {
		t.Fatalf("manual wake released startup accounting barrier: %+v", e)
	}
	noCall(t, h)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAdmissionRemapOnlyChangesLocalTaskReference(t *testing.T) {
	a := task.ResumeAdmission{ID: "task-resume:original-input", TaskID: "12", Epoch: 17}
	in := ProjectTransfer{Schema: 1, Project: "p", Exchanges: map[string][]TransferExchange{
		"console:c": {{Exchange: Exchange{ID: "e-original", Conversation: "console:c", ExpectedTask: "12", Key: a.ID, ResumeAdmission: a}}},
	}}
	out, err := in.Remap(func(id string) string { return "hub~" + id }, func(string) string { return "console:destination" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := out.Exchanges["console:destination"][0]
	if e.ID != "e-original" || e.Key != a.ID || e.ResumeAdmission.ID != a.ID || e.ResumeAdmission.Epoch != a.Epoch ||
		e.ExpectedTask != "hub~12" || e.ResumeAdmission.TaskID != e.ExpectedTask {
		t.Fatalf("remap changed input identity or detached grant from its task: %+v", e)
	}
	if in.Exchanges["console:c"][0].ResumeAdmission != a {
		t.Fatal("remap changed source admission")
	}
}
