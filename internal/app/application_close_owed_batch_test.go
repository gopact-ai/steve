package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/state"
)

func TestOwedClosePassSettlesOneBatchOrKeepsAll(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			f := newOwedCloseFixture(t)
			for i := 1; i < 4; i++ {
				r := f.record
				r.ID, r.Session = fmt.Sprint("attempt-", i), fmt.Sprint("ns_", i)
				if _, err := f.book.Begin(t.Context(), r.ID, "attempt", string(r.State), "", r); err != nil {
					t.Fatal(err)
				}
				conv := fmt.Sprint("c", i)
				s := state.Session{ConversationID: conv, AgentID: "mock", NodeID: r.Node, HarnessID: r.Harness, UpstreamID: r.Session}
				if err := f.store.SaveSession(s); err != nil {
					t.Fatal(err)
				}
				owed := f.owed
				owed.AttemptID, owed.UpstreamID = r.ID, r.Session
				if err := f.store.ArchiveSessionOwingClose(conv, "mock", owed.OwedAt, owed); err != nil {
					t.Fatal(err)
				}
			}
			replica := &closeSettlementReplicator{book: f.book}
			if reject {
				replica.reject = errors.New("quorum unavailable")
			}
			if err := f.book.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			err := f.closes.Reconcile(t.Context())
			if reject && !errors.Is(err, replica.reject) {
				t.Fatalf("rejected batch = %v", err)
			}
			if !reject && err != nil {
				t.Fatal(err)
			}
			if sent := len(f.node.sent()); sent != 4 {
				t.Fatalf("sent %d closes, want 4", sent)
			}
			if writes := replica.writes.Load(); writes != 1 {
				t.Errorf("pass used %d settlement writes, want one", writes)
			}
			want := 0
			if reject {
				want = 4
			}
			if got := len(f.store.OwedCloses()); got != want {
				t.Fatalf("remaining obligations = %d, want %d", got, want)
			}
		})
	}
}
