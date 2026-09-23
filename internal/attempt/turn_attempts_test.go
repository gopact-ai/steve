package attempt

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestForTurnListsEveryAttemptOfTheTurnNewestFirst(t *testing.T) {
	s := identityStore(t)
	insertIdentityRecord(t, s, "first", "bound", `{"id":"first","task_id":"a","turn_id":"turn","started_at":"2026-09-01T00:00:00Z"}`)
	insertIdentityRecord(t, s, "second", "running", `{"id":"second","task_id":"b","turn_id":"turn","started_at":"2026-09-01T00:00:02Z"}`)
	insertIdentityRecord(t, s, "tie", "failed", `{"id":"tie","task_id":"a","turn_id":"turn","started_at":"2026-09-01T00:00:02Z"}`)
	insertIdentityRecord(t, s, "other", "bound", `{"id":"other","task_id":"a","turn_id":"other","started_at":"2026-09-01T00:00:05Z"}`)
	got, err := s.ForTurn(t.Context(), "turn")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"tie", "second", "first"}) {
		t.Fatalf("turn attempts = %v", ids)
	}
	latest, found, err := s.LatestForTurn(t.Context(), "turn")
	if err != nil || !found || latest.ID != ids[0] {
		t.Fatalf("latest=%+v found=%v err=%v; list=%v", latest, found, err, ids)
	}
	none, err := s.ForTurn(t.Context(), "missing")
	if err != nil || len(none) != 0 {
		t.Fatalf("missing turn = %+v %v", none, err)
	}
}

func TestForTurnSeeksTheTurnIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	fixture := seedSettledHistory(t, book, 2000)
	got, err := New(book).ForTurn(t.Context(), fixture.turnID)
	if err != nil || len(got) != 1 || got[0].TurnID != fixture.turnID {
		t.Fatalf("turn attempts = %+v %v", got, err)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if plan := queryPlan(t, diagnostic, turnAttemptsSQL, fixture.turnID); !strings.Contains(plan, "operations_attempt_turn") || strings.Contains(plan, "SCAN operations\n") {
		t.Fatalf("unbounded query plan:\n%s", plan)
	}
}

func TestTurnReadsAndReadabilityCheckFailOnMalformedHistory(t *testing.T) {
	for _, raw := range []string{`null`, `{`, `{"id":"different"}`, `{"id":"bad","usage":{"input":"invalid"}}`} {
		t.Run(raw, func(t *testing.T) {
			s := identityStore(t)
			insertIdentityRecord(t, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn"}`)
			if err := s.CheckReadable(t.Context()); err != nil {
				t.Fatalf("readable history rejected: %v", err)
			}
			insertIdentityRecord(t, s, "bad", "bound", raw)
			if _, err := s.ForTurn(t.Context(), "turn"); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("turn read hid unclassifiable row: %v", err)
			}
			if err := s.CheckReadable(t.Context()); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("readability check hid unclassifiable row: %v", err)
			}
		})
	}
}
