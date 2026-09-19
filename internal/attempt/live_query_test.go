package attempt

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestLiveQueryUsesBoundedHistoryIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(book)
	seedLiveHistory(t, s.l, 10000)
	live, err := s.Live(t.Context())
	if err != nil || len(live) != 2 {
		t.Fatalf("live=%+v error=%v", live, err)
	}
	// Query planning is part of the resource contract: a fixed live set must
	// not read every settled attempt as history grows.
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	rows, err := diagnostic.Query("EXPLAIN QUERY PLAN " + liveAttemptQuery)
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
	if !strings.Contains(plan, "operations_live_attempts") || strings.Contains(plan, "SCAN operations\n") {
		t.Fatalf("unbounded query plan:\n%s", plan)
	}
}

func seedLiveHistory(t testing.TB, l *ledger.Ledger, n int) {
	t.Helper()
	_, err := l.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?) INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at) SELECT 'old-'||n,'attempt','bound',1,1,json_object('id','old-'||n),'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`, n)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, state, data string }{{"running", "running", `{"id":"running"}`}, {"unsettled", "failed", `{"id":"unsettled","unsettled":true}`}} {
		if _, err := l.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, row.id, row.state, row.data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveQueryDoesNotHideMalformedClosedAttempt(t *testing.T) {
	s, _ := newService(t)
	if _, err := s.l.DB().Exec(`INSERT INTO operations VALUES('bad','attempt','bound',1,1,'{','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Live(t.Context()); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("malformed attempt hidden: %v", err)
	}
}

func BenchmarkLiveFixedWorkGrowingHistory(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			l, err := ledger.Open(b.TempDir(), ledger.Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			seedLiveHistory(b, l, n)
			s := New(l)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.Live(b.Context())
				if err != nil || len(got) != 2 {
					b.Fatalf("live=%d err=%v", len(got), err)
				}
			}
		})
	}
}
