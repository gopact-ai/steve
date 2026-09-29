package state

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

// stateReplicator commits every write through consensus as a cluster
// coordinator does, keeping each proposal so a test can count them.
type stateReplicator struct {
	book     *ledger.Ledger
	payloads [][]byte
	reject   error
}

func (r *stateReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *stateReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.payloads = append(r.payloads, append([]byte(nil), write.Payload...))
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func replicatedState(t *testing.T) (*Store, *ledger.Ledger, *stateReplicator) {
	t.Helper()
	book := testLedger(t)
	replicator := &stateReplicator{book: book}
	if err := book.AttachReplication(replicator); err != nil {
		t.Fatal(err)
	}
	store, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	return store, book, replicator
}

func owedSession(t *testing.T, store *Store, conversationID, agentID, upstream string) OwedClose {
	t.Helper()
	if err := store.SaveSession(Session{
		ConversationID: conversationID, AgentID: agentID, HarnessID: "h", NodeID: "node-b", UpstreamID: upstream,
		Workspace: "/w", CapabilityHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}
	return OwedClose{NodeID: "node-b", HarnessID: "h", UpstreamID: upstream, NativeContext: "native-" + upstream, TaskID: "7", AttemptID: "attempt-" + upstream, OwedAt: "2026-09-30T10:00:00Z"}
}

// Letting go of a session whose node could not be reached archives it and
// records the close still owed in one write of the replicated state: a
// coordinator that takes over, or one that restarts, finds both or neither.
func TestArchiveOwingCloseIsOneReplicatedWrite(t *testing.T) {
	store, book, replicator := replicatedState(t)
	owed := owedSession(t, store, "c1", "agent", "ns_1")
	before := len(replicator.payloads)
	if err := store.ArchiveSessionOwingClose("c1", "agent", "2026-09-30T10:00:00Z", owed); err != nil {
		t.Fatal(err)
	}
	if writes := len(replicator.payloads) - before; writes != 1 {
		t.Fatalf("archiving and owing the close took %d replicated writes, want 1", writes)
	}
	snapshot, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replica.Close() })
	if err := replica.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := replica.AttachReplication(&stateReplicator{book: replica}); err != nil {
		t.Fatal(err)
	}
	next, err := OpenLedger(replica)
	if err != nil {
		t.Fatal(err)
	}
	if _, live := next.Conversation("c1").Sessions["agent"]; live {
		t.Fatal("the next coordinator still finds the session live")
	}
	if archived := next.ArchivedSessions("c1", "agent"); len(archived) != 1 || archived[0].UpstreamID != "ns_1" {
		t.Fatalf("the next coordinator finds the history %+v", archived)
	}
	if got := next.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{owed}) {
		t.Fatalf("the next coordinator finds closes owed %+v, want %+v", got, []OwedClose{owed})
	}
	if err := next.SettleOwedClose(owed); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(replica)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.OwedCloses(); len(got) != 0 {
		t.Fatalf("a settled close is still owed after a restart: %+v", got)
	}
}

// A write the cluster refuses leaves the session live and owes nothing.
func TestRefusedArchiveOwingCloseChangesNothing(t *testing.T) {
	store, book, replicator := replicatedState(t)
	owed := owedSession(t, store, "c1", "agent", "ns_1")
	replicator.reject = errors.New("no quorum")
	if err := store.ArchiveSessionOwingClose("c1", "agent", "2026-09-30T10:00:00Z", owed); err == nil {
		t.Fatal("a refused write was reported as done")
	}
	replicator.reject = nil
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Store{"in memory": store, "reopened": reopened} {
		if session, live := s.Conversation("c1").Sessions["agent"]; !live || session.UpstreamID != "ns_1" {
			t.Fatalf("%s: the session is not live after a refused write: %+v", name, s.Conversation("c1"))
		}
		if got := s.ArchivedSessions("c1", "agent"); len(got) != 0 {
			t.Fatalf("%s: archived %+v after a refused write", name, got)
		}
		if got := s.OwedCloses(); len(got) != 0 {
			t.Fatalf("%s: owes %+v after a refused write", name, got)
		}
	}
}

// The close owed must name the session being archived; anything else is
// refused whole.
func TestArchiveOwingCloseRefusesAnotherSession(t *testing.T) {
	for name, change := range map[string]func(*OwedClose){
		"another native session": func(o *OwedClose) { o.UpstreamID = "ns_other" },
		"another node":           func(o *OwedClose) { o.NodeID = "node-c" },
		"another harness":        func(o *OwedClose) { o.HarnessID = "other" },
		"no execution":           func(o *OwedClose) { o.AttemptID = "" },
		"no task":                func(o *OwedClose) { o.TaskID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			store := archiveStore(t)
			owed := owedSession(t, store, "c1", "agent", "ns_1")
			change(&owed)
			if err := store.ArchiveSessionOwingClose("c1", "agent", "2026-09-30T10:00:00Z", owed); err == nil {
				t.Fatal("a close owed for something else was accepted")
			}
			if _, live := store.Conversation("c1").Sessions["agent"]; !live {
				t.Fatal("the session was archived anyway")
			}
			if got := store.OwedCloses(); len(got) != 0 {
				t.Fatalf("owes %+v", got)
			}
		})
	}
	store := archiveStore(t)
	owed := owedSession(t, store, "c1", "agent", "ns_1")
	if err := store.ArchiveSessionOwingClose("c1", "missing", "2026-09-30T10:00:00Z", owed); err == nil {
		t.Fatal("a close was owed for an agent with no live session")
	}
}

// Settling forgets exactly the close it names; a close owed again for the
// same session replaces the one owed before.
func TestSettleOwedCloseForgetsOnlyThatClose(t *testing.T) {
	store := archiveStore(t)
	first := owedSession(t, store, "c1", "agent", "ns_1")
	if err := store.ArchiveSessionOwingClose("c1", "agent", "2026-09-30T10:00:00Z", first); err != nil {
		t.Fatal(err)
	}
	second := owedSession(t, store, "c2", "agent", "ns_2")
	if err := store.ArchiveSessionOwingClose("c2", "agent", "2026-09-30T10:01:00Z", second); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.AttemptID = "attempt-other"
	if err := store.SettleOwedClose(stale); err != nil {
		t.Fatal(err)
	}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{first, second}) {
		t.Fatalf("settling another execution's close changed %+v", got)
	}
	if err := store.SettleOwedClose(first); err != nil {
		t.Fatal(err)
	}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{second}) {
		t.Fatalf("after settling one close, owed %+v, want %+v", got, []OwedClose{second})
	}
	again := owedSession(t, store, "c3", "agent", "ns_2")
	again.AttemptID = "attempt-later"
	if err := store.ArchiveSessionOwingClose("c3", "agent", "2026-09-30T10:02:00Z", again); err != nil {
		t.Fatal(err)
	}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{again}) {
		t.Fatalf("a session owed a close twice is owed %+v, want %+v", got, []OwedClose{again})
	}
}

// A session put back in the conversation is live again: closing it later
// would end the context the user just chose to return to.
func TestRestoredSessionIsNoLongerOwedAClose(t *testing.T) {
	book := testLedger(t)
	store, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	kept := owedSession(t, store, "c2", "agent", "ns_2")
	if err := store.ArchiveSessionOwingClose("c2", "agent", "2026-09-30T10:00:00Z", kept); err != nil {
		t.Fatal(err)
	}
	owed := owedSession(t, store, "c1", "agent", "ns_1")
	if err := store.ArchiveSessionOwingClose("c1", "agent", "2026-09-30T10:01:00Z", owed); err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreSession("c1", "agent", 1)
	if err != nil || restored.UpstreamID != "ns_1" {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{kept}) {
		t.Fatalf("after restoring ns_1, owed %+v, want %+v", got, []OwedClose{kept})
	}
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.OwedCloses(); !reflect.DeepEqual(got, []OwedClose{kept}) {
		t.Fatalf("after a restart, owed %+v, want %+v", got, []OwedClose{kept})
	}
}
