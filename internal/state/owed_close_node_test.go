package state

import (
	"errors"
	"testing"
)

func TestForgetOwedClosesOnOneNodeIsOneReplicatedWrite(t *testing.T) {
	store, _, replicator := replicatedState(t)
	for _, conv := range []string{"one", "two", "other"} {
		owed := owedSession(t, store, conv, "agent", "ns_"+conv)
		if conv == "other" {
			session := store.Conversation(conv).Sessions["agent"]
			session.NodeID, owed.NodeID = "node-other", "node-other"
			if err := store.SaveSession(session); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ArchiveSessionOwingClose(conv, "agent", owed.OwedAt, owed); err != nil {
			t.Fatal(err)
		}
	}
	cleaner, ok := any(store).(interface{ ForgetOwedClosesOn(string) error })
	if !ok {
		t.Fatal("node removal cannot forget its closes owed")
	}
	before := len(replicator.payloads)
	if err := cleaner.ForgetOwedClosesOn("node-b"); err != nil {
		t.Fatal(err)
	}
	remaining := store.OwedCloses()
	if len(remaining) != 1 || remaining[0].NodeID != "node-other" || len(replicator.payloads) != before+1 {
		t.Fatalf("removal: remaining=%+v writes=%d", remaining, len(replicator.payloads)-before)
	}
	if err := cleaner.ForgetOwedClosesOn("node-b"); err != nil || len(replicator.payloads) != before+1 {
		t.Fatalf("repeated cleanup wrote again: %v", err)
	}
}

func TestForgetOwedClosesKeepsThemWhenTheWriteIsRefused(t *testing.T) {
	store, book, replicator := replicatedState(t)
	owed := owedSession(t, store, "one", "agent", "ns_one")
	if err := store.ArchiveSessionOwingClose("one", "agent", owed.OwedAt, owed); err != nil {
		t.Fatal(err)
	}
	replicator.reject = errors.New("not coordinator")
	cleaner, ok := any(store).(interface{ ForgetOwedClosesOn(string) error })
	if !ok {
		t.Fatal("node removal cannot forget its closes owed")
	}
	if err := cleaner.ForgetOwedClosesOn("node-b"); err == nil {
		t.Fatal("refused cleanup reported success")
	}
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{store, reopened} {
		if got := s.OwedCloses(); len(got) != 1 || got[0] != owed {
			t.Fatalf("refused cleanup lost close owed: %+v", got)
		}
	}
}
