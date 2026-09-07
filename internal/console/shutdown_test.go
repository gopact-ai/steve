package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestShutdownWaitsForWrappersAndRetainsUnstartedQueue(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first := enqueueForTest(t, s, "a", "running")
	call := nextCall(t, h)
	queued := enqueueForTest(t, s, "a", "waiting")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown pretended handler settled: %v", err)
	}
	select {
	case <-call.canceled:
	default:
		t.Fatal("shutdown did not cancel running work")
	}
	if _, err := s.Enqueue(t.Context(), "a", "late", nil); !errors.Is(err, consoleapi.ErrConsoleClosing) {
		t.Fatalf("admission remained open: %v", err)
	}
	if _, err := s.Steer(t.Context(), queued.ID); !errors.Is(err, consoleapi.ErrConsoleClosing) {
		t.Fatalf("steer bypassed closed admission: %v", err)
	}
	call.finish <- nil
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := awaitExchange(t, s, first.ID); got.State != "failed" {
		t.Fatalf("terminal receipt missing: %+v", got)
	}
	noCall(t, h)
	for _, e := range s.Queue("a") {
		if e.ID == queued.ID && e.State != "queued" {
			t.Fatalf("unstarted queue was discarded: %+v", e)
		}
	}
	restartedHandler := &queueHandler{started: make(chan *queueCall, 1)}
	restarted := New(restartedHandler, "owner", nil)
	if err := restarted.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Drain(); err != nil {
		t.Fatal(err)
	}
	next := nextCall(t, restartedHandler)
	if next.req.Input != "waiting" {
		t.Fatalf("wrong restart work %q", next.req.Input)
	}
	next.finish <- nil
	_ = awaitExchange(t, restarted, queued.ID)
}
