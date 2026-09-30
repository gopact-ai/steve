package admin

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
)

func TestRemoveNodeForgetsItsClosesOnlyAfterConfigurationCommits(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "refused"}[fail], func(t *testing.T) {
			admin := nodeAdminFixture(t)
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = book.Close() })
			store, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			admin.OwedCloses = store
			for _, node := range []string{"node-test", "node-other"} {
				session := state.Session{ConversationID: node, AgentID: "agent", NodeID: node, HarnessID: "mock", UpstreamID: "ns_" + node}
				if err := store.SaveSession(session); err != nil {
					t.Fatal(err)
				}
				owed := state.OwedClose{NodeID: node, HarnessID: "mock", UpstreamID: session.UpstreamID, TaskID: "1", AttemptID: "a1"}
				if err := store.ArchiveSessionOwingClose(node, "agent", "now", owed); err != nil {
					t.Fatal(err)
				}
			}
			if fail {
				admin.WriteConfig = func(string, *config.Config) error { return errors.New("disk full") }
			}
			err = admin.RemoveNode(t.Context(), "node-test")
			if (err != nil) != fail {
				t.Fatalf("remove = %v", err)
			}
			want := 1
			if fail {
				want = 2
			}
			got := store.OwedCloses()
			if len(got) != want || (!fail && got[0].NodeID != "node-other") {
				t.Fatalf("close obligations after remove: %+v", got)
			}
		})
	}
}
