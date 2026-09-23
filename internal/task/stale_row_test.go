package task

import (
	"testing"
	"time"
)

// staleRow leaves a task the way a stop and resume leave it when the turn
// that opened a row never got as far as binding it: the row is open under
// an epoch the stop revoked, and the task is running again.
func staleRow(t *testing.T, s *Store, bound bool) Task {
	t.Helper()
	r, _ := s.Create(Task{Member: "root"})
	if _, err := s.Begin(r.ID, "root", "", ""); err != nil {
		t.Fatal(err)
	}
	if bound {
		token, err := s.ExecutionToken(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.BindAttempt(token, "att-old", "turn-old"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetAside(r.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Advance(r.ID, StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Attempts[0].ExecutionEpoch == resumed.ExecutionEpoch {
		t.Fatalf("row epoch %d was not revoked", resumed.Attempts[0].ExecutionEpoch)
	}
	return resumed
}

// Nobody can bind an unbound row whose epoch was revoked, so it can never
// be settled by a receipt; Begin closes it as a turn that never ran instead
// of leaving the task unable to run again.
func TestBeginClosesAStaleUnboundRowAsUnstarted(t *testing.T) {
	s, clock := newStore(t)
	tracked := staleRow(t, s, false)
	*clock = clock.Add(time.Hour)
	begun, err := s.Begin(tracked.ID, "root", "", "")
	if err != nil {
		t.Fatalf("stale unbound row blocked the next turn: %v", err)
	}
	if len(begun.Attempts) != 2 {
		t.Fatalf("rows = %+v", begun.Attempts)
	}
	stale, current := begun.Attempts[0], begun.Attempts[1]
	if stale.Open() || stale.Outcome != OutcomeInterrupted || !stale.EndedAt.Equal(stale.StartedAt) {
		t.Fatalf("stale row = %+v", stale)
	}
	if !current.Open() || current.ExecutionEpoch != begun.ExecutionEpoch {
		t.Fatalf("new row = %+v", current)
	}
	if begun.Budget.Turns != 2 || begun.Budget.Elapsed != 0 {
		t.Fatalf("the stale row was charged for time it never ran: %+v", begun.Budget)
	}
}

func TestBeginRefusesAnOpenRowItCannotProveDead(t *testing.T) {
	t.Run("current epoch", func(t *testing.T) {
		s, _ := newStore(t)
		r, _ := s.Create(Task{Member: "root"})
		if _, err := s.Begin(r.ID, "root", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(r.ID, "root", "", ""); err == nil {
			t.Fatal("opened a second row next to a live one")
		}
	})
	t.Run("stale bound row", func(t *testing.T) {
		s, _ := newStore(t)
		tracked := staleRow(t, s, true)
		if _, err := s.Begin(tracked.ID, "root", "", ""); err == nil {
			t.Fatal("closed a row an execution is bound to")
		}
		kept, _ := s.Get(tracked.ID)
		if len(kept.Attempts) != 1 || !kept.Attempts[0].Open() {
			t.Fatalf("rows = %+v", kept.Attempts)
		}
	})
}

// The row keeps the epoch it was opened under: a token of the resumed
// epoch cannot adopt it, so the stale row really is nobody's.
func TestBindAttemptRefusesARowOfARevokedEpoch(t *testing.T) {
	s, _ := newStore(t)
	tracked := staleRow(t, s, false)
	token, err := s.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttempt(token, "att-new", "turn-new"); err == nil {
		t.Fatal("a resumed epoch adopted the revoked row")
	}
}
