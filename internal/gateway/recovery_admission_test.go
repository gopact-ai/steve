package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

type admittedRecoveryProbe struct {
	recoveryProbe
	tasks *task.Store
}

func (p *admittedRecoveryProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if _, err := p.tasks.BeginTurn(req.ExpectedTask, "worker", "", task.TurnInput{
		Address:      channel.Address{Channel: "feishu", Conversation: req.ConversationID, Message: req.MessageID},
		Continuation: true, ResumeAdmission: req.ResumeAdmission, TurnID: req.MessageID,
	}); err != nil {
		return turn.Result{}, err
	}
	if req.OnTurnReady != nil {
		req.OnTurnReady(req.ExpectedTask, "original-attempt")
	}
	p.calls.Add(1)
	_, err := p.tasks.Finish(req.ExpectedTask, task.OutcomeOK, task.Tokens{}, 0)
	return turn.Result{Text: "complete original result", Attempt: "original-attempt"}, err
}

func TestGatewayResumeInputCannotBorrowLaterAuthorization(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "feishu", Channel: "conversation", Member: "worker", Requester: "owner", State: task.StatePaused})
	if err != nil {
		t.Fatal(err)
	}
	p, ch := &admittedRecoveryProbe{tasks: tasks}, &recoveryChannel{}
	g := New(p)
	g.SetRecoveryLedger(book)
	g.BindChannel(ch)
	r := revivalFixture()
	r.TaskID = tracked.ID
	old := task.ResumeAdmission{ID: "old-request", TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	if err := g.QueueTaskResume(t.Context(), book, old.ID, r, old); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); !errors.Is(err, task.ErrResumePending) {
		t.Fatalf("dormant acceptance not visibly pending: %v", err)
	}
	if p.calls.Load() != 0 || ch.notices.Load() != 0 {
		t.Fatal("accepted input ran before task authorization")
	}
	// The first owner's CAS did not commit; another request authorizes a
	// distinct input at the same candidate epoch.
	newer := task.ResumeAdmission{ID: "new-request", TaskID: tracked.ID, Epoch: old.Epoch}
	if _, err := tasks.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, newer); err != nil {
		t.Fatal(err)
	}
	if err := g.QueueTaskResume(t.Context(), book, newer.ID, r, newer); err != nil {
		t.Fatal(err)
	}
	if err := g.QueueTaskResume(t.Context(), book, old.ID, r, newer); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("accepted input rebound to a later admission: %v", err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("obsolete input no longer visible: %v", err)
	}
	if p.calls.Load() != 1 || ch.notices.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("later grant dispatched wrong inputs: calls=%d notices=%d results=%d", p.calls.Load(), ch.notices.Load(), ch.results.Load())
	}
	pending, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(pending) != 1 || pending[0].ID != old.ID {
		t.Fatalf("ungranted input falsely acknowledged: %+v %v", pending, err)
	}
}

func TestGatewayConsumedResumeRecoversDispatchReceiptNotPrompt(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "feishu", Channel: "conversation", Member: "worker", State: task.StatePaused})
	if err != nil {
		t.Fatal(err)
	}
	a := task.ResumeAdmission{ID: "request", TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	if _, err := tasks.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, a); err != nil {
		t.Fatal(err)
	}
	p, ch := &admittedRecoveryProbe{tasks: tasks}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	r := revivalFixture()
	r.TaskID = tracked.ID
	if err := g.QueueTaskResume(t.Context(), book, a.ID, r, a); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_resume_receipt BEFORE UPDATE ON commands
		WHEN NEW.kind='gateway-recovery-dispatch' BEGIN SELECT RAISE(ABORT,'dispatch receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err == nil {
		t.Fatal("receipt failure hidden")
	}
	if err := tasks.CheckResumeAdmission(a); !errors.Is(err, task.ErrResumeConsumed) {
		t.Fatalf("execution did not consume original grant: %v", err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_resume_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("consumed input replayed or stranded: calls=%d resumes=%d results=%d", p.calls.Load(), p.resumes.Load(), ch.results.Load())
	}
}

func TestGatewayManualWakeDoesNotDispatchPendingStartupAccounting(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "feishu", Channel: "conversation", Member: "worker", State: task.StatePaused})
	if err != nil {
		t.Fatal(err)
	}
	a := task.ResumeAdmission{ID: "manual", TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	p, ch := &admittedRecoveryProbe{tasks: tasks}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	// Application recovery has durably accepted this startup input, but its
	// accounting pass has not yet succeeded. Another manual wake cannot
	// consume it merely because both inputs belong to the same gateway.
	if err := g.QueueRecovery(t.Context(), book, "startup-waits-for-accounting", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	r := revivalFixture()
	r.TaskID, r.Manual = tracked.ID, true
	if err := g.QueueTaskResume(t.Context(), book, a.ID, r, a); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, a); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.DispatchResume(t.Context(), book, a, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 1 || ch.notices.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("manual wake drained unrelated startup input: calls=%d notices=%d results=%d", p.calls.Load(), ch.notices.Load(), ch.results.Load())
	}
	if _, exists, err := book.CommandReceipt(t.Context(), "startup-waits-for-accounting/notice"); err != nil || exists {
		t.Fatalf("startup barrier released by manual wake: notice=%v err=%v", exists, err)
	}
}
