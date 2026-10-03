package state

import (
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRecoveryContextsUseQualifiedMetadataInLiveAndArchive(t *testing.T) {
	for _, tc := range []struct {
		name, directory, alias, different string
	}{
		{"drive", `C:\root\work`, `C:/root/temp/../work`, `D:\root\work`},
		{"unc", `\\server\share\work`, `\\server\share\temp\..\work`, `\\server\other\work`},
		{"posix", `/srv/work`, `/srv/temp/../work`, `/srv/other`},
		{"literal-backslash", `/srv/a\b`, `/srv/./a\b`, `/srv/a/b`},
		{"literal-parent", `/srv/a\..\b`, `/srv/./a\..\b`, `/srv/b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := testLedger(t)
			store, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			for _, one := range []struct{ id, node, directory string }{
				{"live", "node", tc.alias},
				{"archived", "node", tc.alias},
				{"other-path", "node", tc.different},
				{"other-node", "other", tc.alias},
				{"empty", "node", ""},
			} {
				if err := store.SaveSession(Session{
					ConversationID: one.id, AgentID: "agent", HarnessID: "h",
					UpstreamID: one.id, NodeID: one.node, Workspace: one.directory,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.ArchiveSession("archived", "agent", "2026-10-03T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				contexts, err := RecoveryContextsTx(tx, "node", tc.directory)
				if err != nil {
					return err
				}
				var ids []string
				for _, one := range contexts {
					ids = append(ids, one.UpstreamID)
				}
				slices.Sort(ids)
				if !slices.Equal(ids, []string{"archived", "live"}) {
					t.Fatalf("live/archive enumeration=%v, want archived and live only", ids)
				}
				empty, err := RecoveryContextsTx(tx, "node", "")
				if err == nil && len(empty) != 0 {
					t.Fatal("empty cwd enumerated contexts")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
