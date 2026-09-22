package attempt

import (
	"strings"
	"testing"
)

func TestIdentityReadsRejectCorruptLedgerEnvelope(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, field := range []string{"revision", "incarnation", "created_at", "updated_at"} {
			name := "unrelated/" + field
			if selected {
				name = "selected/" + field
			}
			t.Run(name, func(t *testing.T) {
				s := identityStore(t)
				insertIdentityRecord(t, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn"}`)
				insertIdentityRecord(t, s, "bad", "bound", `{"id":"bad","task_id":"other","turn_id":"other"}`)
				id := "bad"
				if selected {
					id = "wanted"
				}
				if _, err := s.l.DB().Exec(`UPDATE operations SET `+field+`='not-valid' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
				if _, err := s.l.Operations(t.Context(), "attempt", ""); err == nil {
					t.Fatal("fixture did not violate the ledger owner decoder")
				}
				if _, err := s.ForTask(t.Context(), "task"); err == nil || !strings.Contains(err.Error(), id) {
					t.Fatalf("ForTask hid corrupt %s envelope: %v", id, err)
				}
				if _, _, err := s.LatestForTurn(t.Context(), "turn"); err == nil || !strings.Contains(err.Error(), id) {
					t.Fatalf("LatestForTurn hid corrupt %s envelope: %v", id, err)
				}
				if id, found := s.LiveAttemptOf(t.Context(), "task"); found {
					t.Fatalf("LiveAttemptOf accepted corrupt ledger state: %s", id)
				}
				if _, _, err := s.LatestForSession(t.Context(), "node", "harness", "session"); err == nil {
					t.Fatal("LatestForSession hid a corrupt envelope")
				}
			})
		}
	}
}
