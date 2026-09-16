package task

import (
	"slices"
	"testing"
)

func TestDeleteChannelTakesDelegationsWithIt(t *testing.T) {
	store, _ := newStore(t)
	parent := mustCreate(t, store, "ship the console", "console:one")
	if _, err := store.Begin(parent.ID, "steve", "hub", "sess-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.Finish(parent.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := store.Begin(parent.ID, "steve", "hub", "sess-2"); err != nil {
		t.Fatalf("begin again: %v", err)
	}
	child, err := store.Spawn(parent.ID, Task{Goal: "write the page", Channel: "delegate:one", Member: "builder"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := store.Finish(parent.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatalf("finish again: %v", err)
	}
	other := mustCreate(t, store, "unrelated", "console:two")

	deleted, err := store.DeleteChannel("console:one")
	if err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	if !slices.Equal(deleted, []string{parent.ID, child.ID}) {
		t.Fatalf("deleted = %v; want %v", deleted, []string{parent.ID, child.ID})
	}
	if _, ok := store.Get(parent.ID); ok {
		t.Fatalf("parent %s survived", parent.ID)
	}
	if _, ok := store.Get(child.ID); ok {
		t.Fatalf("delegated child %s survived", child.ID)
	}
	if _, ok := store.Get(other.ID); !ok {
		t.Fatalf("task of another conversation was deleted")
	}
}

func TestDeleteChannelRefusesWorkInFlight(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "ship the console", "console:one")
	if _, err := store.Begin(created.ID, "steve", "hub", "sess-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := store.ChannelIdle("console:one"); err == nil {
		t.Fatalf("called a conversation with an open attempt idle")
	}
	if _, err := store.DeleteChannel("console:one"); err == nil {
		t.Fatalf("deleted a conversation whose task is executing")
	}
	if _, ok := store.Get(created.ID); !ok {
		t.Fatalf("refused delete still removed task %s", created.ID)
	}
}
