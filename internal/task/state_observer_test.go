package task

import (
	"testing"
	"time"
)

func TestStateObserverIgnoresUnchangedStateWrites(t *testing.T) {
	s, err := OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	row := mustCreate(t, s, "goal", "console:main")
	type changed struct {
		id    string
		state State
	}
	events := make(chan changed, 10)
	s.SetStateObserver(func(id string, state State) { events <- changed{id, state} })
	for _, state := range []State{StateRunning, StatePaused, StateRunning, StateCancelled} {
		if _, err := s.Advance(row.ID, state); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-events:
			if got.id != row.ID || got.state != state {
				t.Fatalf("state event = %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatalf("state %s did not notify", state)
		}
		title := "metadata"
		if _, err := s.SetMeta(row.ID, MetaPatch{Title: &title}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-events:
			t.Fatalf("same-state metadata broadcast: %+v", got)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
