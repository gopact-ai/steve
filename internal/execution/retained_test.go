package execution

import (
	"context"
	"errors"
	"testing"
)

func TestRetainedObserverShutdownNeverMeansTaskStopWasConfirmed(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := New(lifetime, nil)
	s, err := r.Begin(t.Context(), Key{AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	unconfirmed := errors.New("native process is still running")
	s.Finish(&RetainedObserverDetached{AttemptID: "attempt-1", NodeID: "node-a", SessionID: "ns_original", Cause: unconfirmed})
	if err := (WaitSet{s}).Wait(t.Context()); !errors.Is(err, unconfirmed) {
		t.Fatalf("explicit stop lost native writer uncertainty: %v", err)
	}
	if err := r.Shutdown(t.Context()); !errors.Is(err, unconfirmed) {
		t.Fatalf("live service ignored unresolved writer: %v", err)
	}
	cancel()
	if err := r.Shutdown(t.Context()); err != nil {
		t.Fatalf("ended service cannot join detached retained observer: %v", err)
	}
	if err := (WaitSet{s}).Wait(t.Context()); !errors.Is(err, unconfirmed) {
		t.Fatalf("global shutdown changed explicit stop evidence: %v", err)
	}
}

func TestRawUnknownWriterIsNeverIgnoredDuringShutdown(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	r := New(lifetime, nil)
	s, err := r.Begin(t.Context(), Key{AttemptID: "raw-1"})
	if err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("raw process not stopped")
	s.Finish(unknown)
	cancel()
	if err := r.Shutdown(t.Context()); !errors.Is(err, unknown) {
		t.Fatalf("raw writer forgotten at shutdown: %v", err)
	}
}

func TestAdoptRetainedReplacesEndedObserverButKeepsCurrentOwnership(t *testing.T) {
	r := New(t.Context(), nil)
	old, _ := r.Begin(t.Context(), Key{AttemptID: "same"})
	old.Finish(errors.New("detached"))
	current, err := r.Begin(t.Context(), Key{AttemptID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	current.AdoptRetained()
	r.mu.Lock()
	_, hasOld := r.entries[old]
	_, hasCurrent := r.entries[current]
	r.mu.Unlock()
	if hasOld || !hasCurrent {
		t.Fatal("reattach lost current execution ownership")
	}
	current.Finish(nil)
}
