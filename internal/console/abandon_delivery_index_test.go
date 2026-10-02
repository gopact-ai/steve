package console

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func TestAbandonmentDeliveryRemainsIndexedAfterPhysicalStop(t *testing.T) {
	s, book, d, r := durableAbandonFixture(t)
	abandonDeliverySQL(t, book, `CREATE TRIGGER refuse_indexed_abandon_receipt BEFORE UPDATE ON bindings WHEN NEW.kind='console-exchange' AND json_extract(NEW.data,'$.recovery_abandon') IS NOT NULL BEGIN SELECT RAISE(ABORT,'receipt refused'); END`)
	out, err := s.Abandon(t.Context(), r.ID, 1)
	if err != nil || !out.Pending {
		t.Fatalf("pending=%+v %v", out, err)
	}
	tracked, _ := d.tasks.Get(r.TaskID)
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, State: nodewire.SessionClosed, ProcessStopped: true, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}}}
	if _, err := d.attempts.ConfirmTaskStopped(t.Context(), r.ID, "original-exit", proof); err != nil {
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
	stops, err := d.attempts.StopCandidates(t.Context())
	if err != nil || len(stops) != 0 {
		t.Fatalf("physical cleanup still pending: %v %v", stops, err)
	}
	pending, err := d.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("physical confirmation dropped receiver obligation: %v %v", pending, err)
	}
	if err := d.tasks.ChannelIdle(t.Context(), tracked.Channel, attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("pending receiver lost task identity: %v", err)
	}
	abandonDeliverySQL(t, book, `DROP TRIGGER refuse_indexed_abandon_receipt`)
	s, book = reopenAbandonReceiver(t, book, d)
	if err := s.ReconcileAbandonments(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.Replies("delivery")) != 1 {
		t.Fatal("reopened indexed obligation was not delivered")
	}
	pending, err = d.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("completed delivery remains in pending index")
	}
	if err := d.tasks.ChannelIdle(t.Context(), tracked.Channel, attempt.CheckTaskDeletionTx); err != nil {
		t.Fatalf("completed receiver still holds task: %v", err)
	}
}

func TestAbandonmentDeliveryReadRejectsInvalidCompletionEvidence(t *testing.T) {
	s, book, d, r := durableAbandonFixture(t)
	if out, err := s.Abandon(t.Context(), r.ID, 1); err != nil || out.Pending {
		t.Fatalf("delivery=%+v %v", out, err)
	}
	abandonDeliverySQL(t, book, `UPDATE operations SET data=json_set(data,'$.abandoned.delivery_result','unrecognized') WHERE id='abandon-delivery'`)
	if _, err := d.PendingAbandonments(t.Context()); err == nil {
		t.Fatal("invalid completed record disappeared from indexed checks")
	}
	var result string
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		return tx.QueryRow(`SELECT json_extract(data,'$.abandoned.delivery_result') FROM operations WHERE id=?`, r.ID).Scan(&result)
	}); err != nil {
		t.Fatal(err)
	}
	if result != "unrecognized" {
		t.Fatal("test did not retain invalid evidence")
	}
}
