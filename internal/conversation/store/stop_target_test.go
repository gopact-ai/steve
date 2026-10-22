package store

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/conversationapi"
)

// A stop bound to a turn is admitted only while that turn has not ended,
// reaches the coordinator with its target, and a retry with the same
// command_id is answered from the original control even after the target
// has ended.
func TestStopTargetIsCheckedAtAdmissionAndReplayed(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	ctx := context.Background()
	first := enqueueForTest(t, s, "main", "first")
	running := nextCall(t, h)
	stop := conversationapi.Submission{Conversation: "main", Input: "/cancel", CommandID: "stop-first", StopTarget: first.ID}
	control, err := s.Submit(ctx, stop)
	if err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.Admission.StopTarget != first.ID {
		t.Fatalf("the control reached the coordinator without its target: %+v", call.req.Admission)
	}
	call.finish <- nil
	awaitExchange(t, s, control.ID)
	running.finish <- nil
	awaitExchange(t, s, first.ID)

	// The next turn starts; the old target has ended.
	second := enqueueForTest(t, s, "main", "second")
	next := nextCall(t, h)
	replayed, err := s.Submit(ctx, stop)
	if err != nil || replayed.ID != control.ID {
		t.Fatalf("retry = %+v, %v; want the original control %s", replayed, err, control.ID)
	}
	late := stop
	late.CommandID = "stop-first-again"
	if _, err := s.Submit(ctx, late); !errors.Is(err, conversationapi.ErrStopTargetEnded) {
		t.Fatalf("a new stop for an ended turn = %v, want ErrStopTargetEnded", err)
	}
	noCall(t, h)
	select {
	case <-next.canceled:
		t.Fatal("a stop for the first turn reached the second")
	default:
	}
	next.finish <- nil
	awaitExchange(t, s, second.ID)
	if _, err := s.Submit(ctx, conversationapi.Submission{Conversation: "main", Input: "hello", CommandID: "x", StopTarget: second.ID}); err == nil {
		t.Fatal("a stop target on a prompt was accepted")
	}
}
