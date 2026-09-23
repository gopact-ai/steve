package attempt

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

// A ledger whose stop candidate index was built on the retired v1 function
// is repaired when it opens: that function is no longer registered, and an
// index still calling it would refuse every write to operations.
func TestStopCandidateIndexReplacesTheRetiredV1Definition(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	const retired = `CREATE INDEX operations_attempt_stop_candidates ON operations(updated_at DESC) WHERE kind = 'attempt' AND steve_attempt_stop_candidate_v1(id, state, data) != 0`
	for _, stmt := range []string{`PRAGMA writable_schema=ON`, `UPDATE sqlite_schema SET sql='` + strings.ReplaceAll(retired, "'", "''") + `' WHERE type='index' AND name='operations_attempt_stop_candidates'`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	var definition string
	if err := book.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE type='index' AND name='operations_attempt_stop_candidates'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(definition, "_v1(") || !strings.Contains(definition, "steve_attempt_stop_candidate_v2(") {
		t.Fatalf("stop candidate index not replaced: %s", definition)
	}
	s := New(book)
	yes := true
	r := Record{Spec: Spec{ID: "owed", TaskID: "t", Kind: KindChat, Node: "n1"}, State: Running, Session: "ns_owed", SessionSettled: &yes}
	putHistoryAttempt(t, book, r)
	if _, err := s.StopCandidates(t.Context()); err != nil {
		t.Fatal(err)
	}
}
