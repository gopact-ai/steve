package state

import "testing"

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
