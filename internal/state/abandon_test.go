package state

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestAbandonedSessionProjectionIsAtomicAndCannotBeRestored(t *testing.T) {
	s, book, replica := replicatedState(t)
	owed := owedSession(t, s, "conversation", "agent", "ns_original")
	replica.reject = errors.New("replication refused")
	if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", owed, true, "", func(*ledger.Tx) error { return nil }); err == nil {
		t.Fatal("refused projection succeeded")
	}
	if len(s.OwedCloses()) != 0 || s.Conversation("conversation").Sessions["agent"].UpstreamID != owed.UpstreamID {
		t.Fatal("refused projection partially installed")
	}
	replica.reject = nil
	if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", owed, true, "", func(*ledger.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", owed, true, "", func(*ledger.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, live := s.Conversation("conversation").Sessions["agent"]; live {
		t.Fatal("abandoned binding remains live")
	}
	if got := s.ArchivedSessions("conversation", "agent"); len(got) != 1 || got[0].AbandonedAttempt != owed.AttemptID {
		t.Fatalf("projection not exact or idempotent: %+v", got)
	}
	if got := s.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{owed}) {
		t.Fatalf("close not owed exactly once: %+v", got)
	}
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.RestoreSession("conversation", "agent", 1); err == nil {
		t.Fatal("abandoned context was restored")
	}
	if !reflect.DeepEqual(s.OwedCloses(), reopened.OwedCloses()) {
		t.Fatal("refused restoration removed the close obligation")
	}
}

func TestAbandonProjectionPreservesANewerBindingAndOwesOnlyTheOldClose(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "already archived"}[archived], func(t *testing.T) {
			s, _, _ := replicatedState(t)
			old := owedSession(t, s, "conversation", "agent", "ns_old")
			if archived {
				if err := s.ArchiveSession("conversation", "agent", old.OwedAt); err != nil {
					t.Fatal(err)
				}
			}
			_ = owedSession(t, s, "conversation", "agent", "ns_new")
			if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", old, true, "", func(*ledger.Tx) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if s.Conversation("conversation").Sessions["agent"].UpstreamID != "ns_new" {
				t.Fatal("projection archived the new binding")
			}
			for _, record := range s.ArchivedSessions("conversation", "agent") {
				if record.UpstreamID != "ns_old" || record.AbandonedAttempt != old.AttemptID {
					t.Fatal("projection changed another archived context")
				}
			}
			if !reflect.DeepEqual(s.OwedCloses(), []OwedClose{old}) {
				t.Fatal("projection did not retain the original close")
			}
		})
	}
}

func TestAbandonedSessionWithExitProofDoesNotInventAClose(t *testing.T) {
	s, _, _ := replicatedState(t)
	owed := owedSession(t, s, "conversation", "agent", "ns_original")
	if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", owed, false, "", func(*ledger.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.OwedCloses()) != 0 {
		t.Fatal("an exited session was owed another close")
	}
	if archived := s.ArchivedSessions("conversation", "agent"); len(archived) != 1 || archived[0].AbandonedAttempt == "" {
		t.Fatal("exited abandoned context remained resumable")
	}
}

func TestLateSessionSaveCannotResurrectAnAbandonedContext(t *testing.T) {
	s, _, _ := replicatedState(t)
	owed := owedSession(t, s, "conversation", "agent", "ns_original")
	original := s.Conversation("conversation").Sessions["agent"]
	if err := s.ProjectAbandonedSession(t.Context(), "conversation", "agent", owed, true, "", func(*ledger.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(original); !errors.Is(err, ErrAbandonedContext) {
		t.Fatalf("late session save=%v", err)
	}
	if _, found := s.Conversation("conversation").Sessions["agent"]; found {
		t.Fatal("retired context was resurrected")
	}
}
