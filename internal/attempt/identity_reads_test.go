package attempt

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func identityStore(t testing.TB) *Service {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return New(book)
}

func insertIdentityRecord(t testing.TB, s *Service, id, state, raw string) {
	t.Helper()
	if _, err := s.l.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, id, state, raw); err != nil {
		t.Fatal(err)
	}
}

func seedIdentityHistory(t testing.TB, s *Service, n int) {
	t.Helper()
	if _, err := s.l.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
		INSERT INTO operations SELECT 'history-'||n,'attempt','bound',1,1,
		json_object('id','history-'||n,'task_id','other-'||n,'turn_id','other-'||n,'started_at','2026-09-01T00:00:00Z'),
		'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`, n); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityReadsDoNotDecodeUnrelatedHistory(t *testing.T) {
	s := identityStore(t)
	insertIdentityRecord(t, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn","started_at":"2026-09-01T00:00:00Z"}`)
	check := func() {
		t.Helper()
		got, err := s.ForTask(t.Context(), "task")
		if err != nil || len(got) != 1 || got[0].ID != "wanted" {
			t.Fatalf("task=%+v err=%v", got, err)
		}
		latest, found, err := s.LatestForTurn(t.Context(), "turn")
		if err != nil || !found || latest.ID != "wanted" {
			t.Fatalf("turn=%+v found=%v err=%v", latest, found, err)
		}
		if id, found, err := s.LiveAttemptOf(t.Context(), "task"); err != nil || !found || id != "wanted" {
			t.Fatalf("live=%s found=%v err=%v", id, found, err)
		}
	}
	before := testing.AllocsPerRun(3, check)
	seedIdentityHistory(t, s, 10000)
	after := testing.AllocsPerRun(3, check)
	t.Logf("fixed three identity reads allocations: %.0f -> %.0f", before, after)
	if after > before+30 {
		t.Fatalf("unrelated history participates in identity reads: %.0f -> %.0f", before, after)
	}
}

func TestIdentityReadsRejectMiskeyedOrMalformedOwnerRows(t *testing.T) {
	for _, raw := range []string{
		`null`, `{`, `{"id":"different","task_id":"other"}`,
		`{"id":"bad","usage":{"input":"invalid"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s := identityStore(t)
			insertIdentityRecord(t, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn"}`)
			insertIdentityRecord(t, s, "bad", "bound", raw)
			if _, err := s.ForTask(t.Context(), "task"); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("task hid unclassifiable owner row: %v", err)
			}
			if _, _, err := s.LatestForTurn(t.Context(), "turn"); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("turn hid unclassifiable owner row: %v", err)
			}
			if id, found, err := s.LiveAttemptOf(t.Context(), "task"); err == nil || found {
				t.Fatalf("live query hid invalid owner: %s found=%v err=%v", id, found, err)
			}
		})
	}
}

func TestIdentityReadsPreserveOwnerJSONAndNanosecondOrder(t *testing.T) {
	s := identityStore(t)
	for _, row := range []struct{ id, raw string }{
		{"early", `{"id":"early","task_id":"task","turn_id":"turn","started_at":"2026-09-01T00:00:00.000000001Z"}`},
		{"later", `{"id":"later","task_id":"ignored","TASK_ID":"task","turn_id":"ignored","turn\u005fid":"turn","started_at":"2026-09-01T08:00:00.000000002+08:00","Unsettled":true}`},
		{"other", `{"id":"other","task_id":"other","turn_id":"other","started_at":"2026-09-02T00:00:00Z"}`},
	} {
		insertIdentityRecord(t, s, row.id, "bound", row.raw)
	}
	got, err := s.ForTask(t.Context(), "task")
	if err != nil || len(got) != 2 || got[0].ID != "early" || got[1].ID != "later" {
		t.Fatalf("task order=%+v err=%v", got, err)
	}
	last, found, err := s.LatestForTurn(t.Context(), "turn")
	if err != nil || !found || last.ID != "later" {
		t.Fatalf("turn order=%+v found=%v err=%v", last, found, err)
	}
	if id, found, err := s.LiveAttemptOf(t.Context(), "task"); err != nil || !found || id != "later" {
		t.Fatalf("terminal unsettled attempt lost: %s %v %v", id, found, err)
	}
	if rows, err := s.ForTask(t.Context(), "absent"); err != nil || len(rows) != 0 {
		t.Fatalf("absent task=%+v err=%v", rows, err)
	}
}

func TestIdentityQueryPlansSeekOwnerKeys(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	for _, tc := range []struct{ query, index, key string }{
		{taskIdentitySQL, "operations_attempt_task", "task"},
		{turnIdentitySQL, "operations_attempt_turn", "turn"},
		{liveTaskIdentitySQL, "operations_attempt_task_live", "task"},
		{sessionIdentitySQL, "operations_attempt_session", sessionIdentityKey("node", "mock", "ns_a")},
		{invalidIdentitySQL, "operations_attempt_task", ""},
	} {
		t.Run(tc.index+tc.key, func(t *testing.T) {
			args := []any{tc.key}
			if tc.query == invalidIdentitySQL {
				args = nil
			}
			rows, err := diagnostic.Query("EXPLAIN QUERY PLAN "+tc.query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan += detail + "\n"
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, "SEARCH operations USING INDEX "+tc.index) ||
				strings.Contains(plan, "SCAN operations") || strings.Contains(plan, "SCAN events") ||
				strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("identity read lost bounded key seek:\n%s", plan)
			}
		})
	}
}

func TestIdentityIndexesRestoreAndAdvanceWithOwnerWrites(t *testing.T) {
	s := identityStore(t)
	insertIdentityRecord(t, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn"}`)
	if _, err := s.l.Transition(t.Context(), "wanted", "running", "bound", "test", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.LiveAttemptOf(t.Context(), "task"); err != nil || found {
		t.Fatalf("terminal transition left live identity: %s err=%v", id, err)
	}
	if _, err := s.l.DB().Exec(`UPDATE operations SET data=json_set(data,'$.Unsettled',json('true')) WHERE id='wanted'`); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.LiveAttemptOf(t.Context(), "task"); err != nil || !found || id != "wanted" {
		t.Fatalf("late unsettled evidence not indexed: %s %v %v", id, found, err)
	}
	if _, err := s.l.DB().Exec(`DROP INDEX operations_attempt_turn`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target := identityStore(t)
	if err := target.l.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	got, found, err := target.LatestForTurn(t.Context(), "turn")
	if err != nil || !found || got.ID != "wanted" {
		t.Fatalf("restored index missing: %+v %v %v", got, found, err)
	}
	if id, found, err := target.LiveAttemptOf(t.Context(), "task"); err != nil || !found || id != "wanted" {
		t.Fatalf("restored unsettled evidence not indexed: %s %v %v", id, found, err)
	}
}

func BenchmarkIdentityReadsFixedWorkGrowingHistory(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := identityStore(b)
			seedIdentityHistory(b, s, n)
			insertIdentityRecord(b, s, "wanted", "running", `{"id":"wanted","task_id":"task","turn_id":"turn"}`)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := s.LatestForTurn(b.Context(), "turn"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
