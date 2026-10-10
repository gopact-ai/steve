package state

import (
	"errors"
	"reflect"
	"testing"
)

func TestSessionRetirementKeepsAReplacement(t *testing.T) {
	for _, deferClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "owed"}[deferClose], func(t *testing.T) {
			store, _, _ := replicatedState(t)
			owed := owedSession(t, store, "conversation", "agent", "ns_original")
			original := store.Conversation("conversation").Sessions["agent"]
			replacement := original
			replacement.UpstreamID = "ns_replacement"
			if err := store.SaveSession(replacement); err != nil {
				t.Fatal(err)
			}
			before := store.Conversation("conversation")
			var debt *OwedClose
			if deferClose {
				debt = &owed
			}
			if err := store.RetireSession(original, owed.OwedAt, debt); !errors.Is(err, ErrSessionChanged) {
				t.Fatalf("retired a replaced session: %v", err)
			}
			if !reflect.DeepEqual(before, store.Conversation("conversation")) || len(store.OwedCloses()) != 0 {
				t.Fatal("retirement changed replacement or invented debt")
			}
		})
	}
}

func TestSessionRetirementRefusedWriteKeepsTheLiveSlot(t *testing.T) {
	for _, deferClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "owed"}[deferClose], func(t *testing.T) {
			store, book, replica := replicatedState(t)
			owed := owedSession(t, store, "conversation", "agent", "ns_original")
			original := store.Conversation("conversation").Sessions["agent"]
			before := store.Conversation("conversation")
			replica.reject = errors.New("test retirement proposal refused")
			var debt *OwedClose
			if deferClose {
				debt = &owed
			}
			if err := store.RetireSession(original, owed.OwedAt, debt); !errors.Is(err, replica.reject) {
				t.Fatalf("retirement = %v", err)
			}
			replica.reject = nil
			reopened, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range []*Store{store, reopened} {
				if !reflect.DeepEqual(before, got.Conversation("conversation")) || len(got.OwedCloses()) != 0 {
					t.Fatal("refused commit changed live state")
				}
			}
		})
	}
}

func TestSessionRetirementRejectsAnotherCloseIdentity(t *testing.T) {
	for _, field := range []string{"node", "harness", "session", "task", "attempt", "at"} {
		t.Run(field, func(t *testing.T) {
			store, _, _ := replicatedState(t)
			owed := owedSession(t, store, "conversation", "agent", "ns_original")
			original := store.Conversation("conversation").Sessions["agent"]
			before := store.Conversation("conversation")
			switch field {
			case "node":
				owed.NodeID = "other"
			case "harness":
				owed.HarnessID = "other"
			case "session":
				owed.UpstreamID = "other"
			case "task":
				owed.TaskID = ""
			case "attempt":
				owed.AttemptID = ""
			case "at":
				owed.OwedAt = "other"
			}
			if err := store.RetireSession(original, "2026-09-30T10:00:00Z", &owed); err == nil {
				t.Fatal("bad debt accepted")
			}
			if !reflect.DeepEqual(before, store.Conversation("conversation")) || len(store.OwedCloses()) != 0 {
				t.Fatal("invalid retirement changed state")
			}
		})
	}
}

func TestSessionRetirementKeepsChangedMetadataWithTheSameNativeID(t *testing.T) {
	for _, field := range []string{"project", "token"} {
		t.Run(field, func(t *testing.T) {
			store, _, _ := replicatedState(t)
			debt := owedSession(t, store, "conversation", "agent", "ns_original")
			original := store.Conversation("conversation").Sessions["agent"]
			changed := original
			switch field {
			case "workspace":
				changed.Workspace = "/changed"
			case "project":
				changed.ProjectVersion++
			case "token":
				changed.AgentToken = "synthetic-new-token"
			}
			if err := store.SaveSession(changed); err != nil {
				t.Fatal(err)
			}
			if err := store.RetireSession(original, debt.OwedAt, &debt); !errors.Is(err, ErrSessionChanged) {
				t.Fatalf("stale retirement = %v", err)
			}
			if !reflect.DeepEqual(changed, store.Conversation("conversation").Sessions["agent"]) || len(store.OwedCloses()) != 0 {
				t.Fatal("changed same-ID context was retired")
			}
		})
	}
}
