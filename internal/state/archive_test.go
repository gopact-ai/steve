package state

import (
	"path/filepath"
	"testing"
)

func archiveStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func save(t *testing.T, store *Store, agentID, upstream string) {
	t.Helper()
	if err := store.SaveSession(Session{
		ConversationID: "c1", AgentID: agentID, HarnessID: "h", UpstreamID: upstream,
		Workspace: "/w", CapabilityHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}
}

// Clearing must end the agent's context without putting the history out of
// reach: the agent session is closed, not deleted, so the record is the only
// way back to it.
func TestArchiveKeepsTheSessionReachable(t *testing.T) {
	store := archiveStore(t)
	save(t, store, "codex", "up-1")
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, live := store.Conversation("c1").Sessions["codex"]; live {
		t.Fatal("archived session should not still be active")
	}
	archived := store.ArchivedSessions("c1", "codex")
	if len(archived) != 1 || archived[0].UpstreamID != "up-1" {
		t.Fatalf("archived = %+v", archived)
	}
}

// A session the agent never named has nothing to go back to.
func TestArchiveSkipsSessionWithNoUpstreamID(t *testing.T) {
	store := archiveStore(t)
	save(t, store, "codex", "")
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := store.ArchivedSessions("c1", "codex"); len(got) != 0 {
		t.Fatalf("archived = %+v, want nothing to act on", got)
	}
}

func TestArchiveListsNewestFirstAndRestores(t *testing.T) {
	store := archiveStore(t)
	for i, up := range []string{"up-1", "up-2", "up-3"} {
		save(t, store, "codex", up)
		if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:0"+string(rune('0'+i))+":00Z"); err != nil {
			t.Fatal(err)
		}
	}
	archived := store.ArchivedSessions("c1", "codex")
	want := []string{"up-3", "up-2", "up-1"}
	for i, id := range want {
		if archived[i].UpstreamID != id {
			t.Fatalf("archived[%d] = %q, want %q", i, archived[i].UpstreamID, id)
		}
	}
	restored, err := store.RestoreSession("c1", "codex", 2)
	if err != nil {
		t.Fatal(err)
	}
	if restored.UpstreamID != "up-2" {
		t.Fatalf("restored %q, want up-2", restored.UpstreamID)
	}
	if got := store.Conversation("c1").Sessions["codex"].UpstreamID; got != "up-2" {
		t.Fatalf("active session = %q", got)
	}
	// Restoring moves rather than copies, so nothing is live and archived at
	// once, and the remaining history is intact.
	left := store.ArchivedSessions("c1", "codex")
	if len(left) != 2 {
		t.Fatalf("archive after restore = %+v", left)
	}
	for _, item := range left {
		if item.UpstreamID == "up-2" {
			t.Fatalf("restored session still in the archive: %+v", left)
		}
	}
}

// Restoring while another session is live must not lose the live one.
func TestRestoreSwapsWithTheLiveSession(t *testing.T) {
	store := archiveStore(t)
	save(t, store, "codex", "old")
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	save(t, store, "codex", "current")
	if _, err := store.RestoreSession("c1", "codex", 1); err != nil {
		t.Fatal(err)
	}
	if got := store.Conversation("c1").Sessions["codex"].UpstreamID; got != "old" {
		t.Fatalf("active = %q, want old", got)
	}
	archived := store.ArchivedSessions("c1", "codex")
	if len(archived) != 1 || archived[0].UpstreamID != "current" {
		t.Fatalf("archive = %+v, want the previously live session", archived)
	}
}

// A restored session has been away; the next turn re-checks drift, so it must
// not come back still marked mid-turn.
func TestRestoreClearsTainted(t *testing.T) {
	store := archiveStore(t)
	if err := store.SaveSession(Session{
		ConversationID: "c1", AgentID: "codex", HarnessID: "h", UpstreamID: "up-1",
		Workspace: "/w", CapabilityHash: "hash", Tainted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreSession("c1", "codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Tainted {
		t.Fatal("restored session should not still be tainted")
	}
}

func TestRestoreRejectsOutOfRange(t *testing.T) {
	store := archiveStore(t)
	save(t, store, "codex", "up-1")
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, -1, 2} {
		if _, err := store.RestoreSession("c1", "codex", index); err != ErrNoArchive {
			t.Errorf("index %d: err = %v, want ErrNoArchive", index, err)
		}
	}
}

func TestArchiveIsBoundedAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxArchived+5; i++ {
		save(t, store, "codex", "up-"+string(rune('a'+i)))
		if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.ArchivedSessions("c1", "codex")); got != maxArchived {
		t.Fatalf("archive size = %d, want %d", got, maxArchived)
	}
	// The oldest go first.
	if store.ArchivedSessions("c1", "codex")[maxArchived-1].UpstreamID == "up-a" {
		t.Fatal("oldest record should have been dropped")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.ArchivedSessions("c1", "codex")); got != maxArchived {
		t.Fatalf("archive after reopen = %d", got)
	}
}

// Two agents in one conversation keep separate histories.
func TestArchiveIsPerAgent(t *testing.T) {
	store := archiveStore(t)
	save(t, store, "codex", "c-1")
	if err := store.ArchiveSession("c1", "codex", "2026-08-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	save(t, store, "claude", "l-1")
	if err := store.ArchiveSession("c1", "claude", "2026-08-26T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := store.ArchivedSessions("c1", "codex"); len(got) != 1 || got[0].UpstreamID != "c-1" {
		t.Fatalf("codex archive = %+v", got)
	}
	if got := store.ArchivedSessions("c1", "claude"); len(got) != 1 || got[0].UpstreamID != "l-1" {
		t.Fatalf("claude archive = %+v", got)
	}
}
