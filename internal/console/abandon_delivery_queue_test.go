package console

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
)

func TestFailedAbandonmentReplyDoesNotReleaseTheQueuedInput(t *testing.T) {
	s, book, _, r := durableAbandonFixture(t)
	h := &queueHandler{started: make(chan *queueCall, 2)}
	s.coordinator = h
	queued, err := s.Enqueue(t.Context(), "delivery", "the next input", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	question := consoleapi.PendingQuestion{ID: "original-question", Conversation: "console:delivery", ExchangeID: "original", State: "pending"}
	s.questions[question.ID] = question
	waiter := make(chan struct{})
	s.questionWaiters[question.ID] = waiter
	err = s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	abandonDeliverySQL(t, book, `CREATE TRIGGER refuse_atomic_abandon_reply BEFORE INSERT ON bindings WHEN NEW.kind='console-reply' BEGIN SELECT RAISE(ABORT,'reply refused'); END`)
	out, err := s.Abandon(t.Context(), r.ID, 1)
	if err != nil || !out.Pending {
		t.Fatalf("pending reply=%+v %v", out, err)
	}
	select {
	case call := <-h.started:
		call.finish <- nil
		t.Fatal("failed abandonment dispatched another input")
	default:
	}
	s.mu.Lock()
	target := s.exchanges["console:delivery"][0]
	running := s.running[target.Conversation]
	q := s.questions[question.ID]
	terminal := target.State.Terminal()
	s.mu.Unlock()
	if terminal || running != 1 || q.State != "pending" {
		t.Fatalf("refused completion changed state: terminal=%v running=%d question=%s", terminal, running, q.State)
	}
	select {
	case <-waiter:
		t.Fatal("failed save released question waiter")
	default:
	}
	select {
	case <-target.done:
		t.Fatal("failed save released exchange waiter")
	default:
	}
	abandonDeliverySQL(t, book, `DROP TRIGGER refuse_atomic_abandon_reply`)
	if err := s.ReconcileAbandonments(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.Input != "the next input" {
		t.Fatalf("dispatched %q", call.req.Input)
	}
	if err := s.ReconcileAbandonments(t.Context()); err != nil {
		call.finish <- nil
		t.Fatal(err)
	}
	select {
	case duplicate := <-h.started:
		duplicate.finish <- nil
		call.finish <- nil
		t.Fatal("duplicate acknowledgement dispatched twice")
	default:
	}
	call.finish <- nil
	awaitExchange(t, s, queued.ID)
	s.workers.Wait()
}

func TestAbandonmentDeliveryAcknowledgementOutlivesTaskAndConversationDeletion(t *testing.T) {
	s, _, d, r := durableAbandonFixture(t)
	out, err := s.Abandon(t.Context(), r.ID, 1)
	if err != nil || out.Pending {
		t.Fatalf("abandonment=%+v %v", out, err)
	}
	tracked, _ := d.tasks.Get(r.TaskID)
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, State: nodewire.SessionClosed, ProcessStopped: true, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}}}
	if _, err := d.attempts.ConfirmTaskStopped(t.Context(), r.ID, "node-exit", proof); err != nil {
		t.Fatal(err)
	}
	if _, err := d.attempts.MarkStopProjected(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, owed := range d.sessions.OwedCloses() {
		if err := d.sessions.SettleOwedClose(owed); err != nil {
			t.Fatal(err)
		}
	}
	guard := func(tx ledger.Reader, ids []string) error {
		if err := attempt.CheckTaskDeletionTx(tx, ids); err != nil {
			return err
		}
		return state.CheckTaskDeletionTx(tx, ids)
	}
	if _, err := d.tasks.DeleteChannel(t.Context(), tracked.Channel, guard); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(tracked.Channel); err != nil {
		t.Fatal(err)
	}
	if err := d.attempts.MarkUnsettled(t.Context(), r.ID, "late-observation", attempt.ErrStopConfirmationRequired, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAbandonments(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.Replies("delivery")) != 0 {
		t.Fatal("late event recreated deleted conversation")
	}
	pending, err := d.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("late event recreated receiver obligation: %v %v", pending, err)
	}
}
