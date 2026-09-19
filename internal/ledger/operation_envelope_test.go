package ledger

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperationEnvelopeIndexMatchesOwnerConversions(t *testing.T) {
	const validTime = "2026-09-19T00:00:00.000000001Z"
	for _, field := range []string{"revision", "incarnation", "created_at", "updated_at"} {
		for _, value := range []any{
			int64(0), int64(-1), int64(12), float64(12), float64(1.5),
			"not-valid", "12", "0012", "+12", "12.5", "18446744073709551615",
			[]byte("12"), []byte("not-valid"), validTime, []byte(validTime),
			"2026-09-19T08:00:00.000000001+08:00",
		} {
			t.Run(fmt.Sprintf("%s/%T/%v", field, value, value), func(t *testing.T) {
				book, err := Open(t.TempDir(), Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer book.Close()
				if _, err := book.Begin(t.Context(), "one", "test", "open", "actor", struct{}{}); err != nil {
					t.Fatal(err)
				}
				if _, err := book.DB().Exec(`UPDATE operations SET `+field+`=? WHERE id='one'`, value); err != nil {
					t.Fatal(err)
				}
				_, _, ownerErr := book.Operation(t.Context(), "one")
				indexErr := book.Read(t.Context(), func(tx *ReadTx) error { return CheckOperationEnvelopesTx(tx, "test") })
				if (ownerErr != nil) != (indexErr != nil) {
					t.Fatalf("index disagrees with ledger owner: owner=%v index=%v", ownerErr, indexErr)
				}
			})
		}
	}
}

func TestOperationEnvelopeGuardUsesBoundedSameSnapshotAndRestores(t *testing.T) {
	dir := t.TempDir()
	book, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if _, err := book.Begin(t.Context(), "one", "attempt", "bound", "actor", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000)
		INSERT INTO operations SELECT 'history-'||x,'attempt','bound',1,1,'{}',
		'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z' FROM n`); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	rows, err := diagnostic.Query("EXPLAIN QUERY PLAN "+invalidOperationEnvelopeQuery, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH operations USING COVERING INDEX operations_invalid_envelopes") &&
		!strings.Contains(plan, "SEARCH operations USING INDEX operations_invalid_envelopes") {
		t.Fatalf("guard scans historical envelopes:\n%s", plan)
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error {
		if err := CheckOperationEnvelopesTx(tx, "attempt"); err != nil {
			return err
		}
		if _, err := diagnostic.Exec(`UPDATE operations SET updated_at='broken' WHERE id='one'`); err != nil {
			return err
		}
		return CheckOperationEnvelopesTx(tx, "attempt")
	}); err != nil {
		t.Fatalf("guard crossed its read snapshot: %v", err)
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error { return CheckOperationEnvelopesTx(tx, "attempt") }); err == nil {
		t.Fatal("new snapshot did not see corruption")
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error { return CheckOperationEnvelopesTx(tx, "unrelated") }); err != nil {
		t.Fatalf("another operation kind was blocked: %v", err)
	}
	if _, err := book.DB().Exec(`UPDATE operations SET updated_at=? WHERE id='one'`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error { return CheckOperationEnvelopesTx(tx, "attempt") }); err != nil {
		t.Fatalf("repair did not remove invalid index entry: %v", err)
	}
	if _, err := book.DB().Exec(`UPDATE operations SET updated_at='broken' WHERE id='one'`); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`DROP INDEX operations_invalid_envelopes`); err != nil {
		t.Fatal(err)
	}
	raw, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(raw); err != nil {
		t.Fatal(err)
	}
	if err := target.Read(t.Context(), func(tx *ReadTx) error { return CheckOperationEnvelopesTx(tx, "attempt") }); err == nil || !strings.Contains(err.Error(), "one") {
		t.Fatalf("restore omitted invalid-envelope guard: %v", err)
	}
}
