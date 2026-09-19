package app

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestSessionCleanupUsesCreationOrderWithoutReadingEventHistory(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	active := cluster.Activation{Context: t.Context(), Ledger: book}
	place := harness.Placement{Node: "worker", Harness: "mock"}
	for _, row := range []struct{ id, node, harness, session, at string }{
		{"old", "worker", "mock", "ns_original", "2026-09-02T00:00:00Z"},
		{"new", "worker", "mock", "ns_original", "2026-09-01T00:00:00Z"},
		{"other-node", "other", "mock", "ns_original", "2026-09-03T00:00:00Z"},
		{"other-harness", "worker", "other", "ns_original", "2026-09-03T00:00:00Z"},
		{"other-session", "worker", "mock", "ns_other", "2026-09-03T00:00:00Z"},
	} {
		at, _ := time.Parse(time.RFC3339Nano, row.at)
		record := attempt.Record{Spec: attempt.Spec{ID: row.id, Node: row.node, Harness: row.harness}, State: attempt.Bound, Session: row.session, StartedAt: at}
		if _, err := book.Begin(t.Context(), row.id, "attempt", "bound", "test", record); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		got, err := sessionCleanupRecord(t.Context(), active, place, "ns_original")
		if err != nil || got.ID != "new" {
			t.Fatalf("cleanup selected wall-clock/foreign binding: %+v err=%v", got, err)
		}
	}
	before := testing.AllocsPerRun(3, check)
	// Real ledger events, but none changes the first creation event. A cleanup
	// identity read must not decode the entire retained progress history.
	if _, err := book.DB().Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000)
		INSERT INTO events(operation_id,revision,incarnation,from_state,to_state,actor,fencings,effects,at)
		SELECT 'old',x+1,1,'bound','bound','test','[]','null','2026-09-01T00:00:00Z' FROM n`); err != nil {
		t.Fatal(err)
	}
	after := testing.AllocsPerRun(3, check)
	t.Logf("cleanup allocations after 10k progress events: %.0f -> %.0f", before, after)
	if after > before+20 {
		t.Fatalf("cleanup reads whole event history: %.0f -> %.0f", before, after)
	}
	if _, err := book.DB().Exec(`DELETE FROM events WHERE operation_id='old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionCleanupRecord(t.Context(), active, place, "ns_original"); err == nil {
		t.Fatal("cleanup hid a matching binding with no creation event")
	}
}

func TestSessionCleanupRefusesAbsentOrInactiveIdentity(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ctx, cancel := context.WithCancel(t.Context())
	active := cluster.Activation{Context: ctx, Ledger: book}
	if _, err := sessionCleanupRecord(t.Context(), active, harness.Placement{}, "ns_absent"); err == nil {
		t.Fatal("missing binding accepted")
	}
	cancel()
	if _, err := sessionCleanupRecord(t.Context(), active, harness.Placement{}, "ns_absent"); err != context.Canceled {
		t.Fatalf("inactive coordinator not rejected: %v", err)
	}
}

func TestSessionCleanupRejectsCorruptSelectedLedgerEnvelope(t *testing.T) {
	for _, field := range []string{"revision", "incarnation", "created_at", "updated_at"} {
		t.Run(field, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			record := attempt.Record{Spec: attempt.Spec{ID: "selected", TaskID: "task", Node: "worker", Harness: "mock"}, Session: "ns_original"}
			if _, err := book.Begin(t.Context(), record.ID, "attempt", "bound", "test", record); err != nil {
				t.Fatal(err)
			}
			if _, err := book.DB().Exec(`UPDATE operations SET ` + field + `='not-valid' WHERE id='selected'`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := book.Operation(t.Context(), record.ID); err == nil {
				t.Fatal("ledger owner accepted invalid test envelope")
			}
			active := cluster.Activation{Context: t.Context(), Ledger: book}
			if got, err := sessionCleanupRecord(t.Context(), active, harness.Placement{Node: "worker", Harness: "mock"}, "ns_original"); err == nil {
				t.Fatalf("cleanup accepted corrupt %s: %+v", field, got)
			}
		})
	}
}
