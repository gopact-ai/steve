package attempt

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestOriginalRecoveryAssociationIndexSeparatesSourcesFromProducersAndLegacyAB(t *testing.T) {
	s, clock := newService(t)
	for _, record := range []Record{
		{Spec: Spec{ID: "original", TaskID: "1"}, State: Failed, Revision: 1, Abandoned: &Abandoned{WorkspaceRecoveryID: "episode", At: clock.t, ForceStopRevision: 1}},
		{Spec: Spec{ID: "producer", TaskID: "2", WorkspaceRecovery: &RecoveryExecution{ID: "episode"}}, State: Bound, Revision: 1},
		{Spec: Spec{ID: "historical-ab", TaskID: "3"}, State: Failed, Revision: 1, Abandoned: &Abandoned{At: clock.t, ForceStopRevision: 1}},
		{Spec: Spec{ID: "copy-ab", TaskID: "4", Workspace: project.Workspace{Kind: project.KindCopy}}, State: Failed, Revision: 1, Abandoned: &Abandoned{At: clock.t, ForceStopRevision: 1}},
	} {
		if _, err := s.l.Begin(t.Context(), record.ID, kind, string(record.State), "fixture", record); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.Read(t.Context(), func(tx *ledger.ReadTx) error {
		associated, err := originalRecoverySourcesTx(tx, "episode")
		if err != nil || len(associated) != 1 || !associated["original"] {
			t.Fatalf("original index mixed other obligations: %+v %v", associated, err)
		}
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM operations INDEXED BY ` + originalRecoveryIndex + ` WHERE kind='attempt' AND ` + originalRecoveryKey + ` IS NULL`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("valid legacy or copy AB became an invalid association")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalRecoveryAssociationReadsUseOnlyTheirIndexRange(t *testing.T) {
	var baseline float64
	for _, size := range []int{24, 2000} {
		dir := t.TempDir()
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { book.Close() })
		s := New(book)
		diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { diagnostic.Close() })
		record := Record{Spec: Spec{ID: "selected", TaskID: "selected-task"}, State: Failed, Revision: 1, Abandoned: &Abandoned{WorkspaceRecoveryID: "selected-episode", At: time.Now().UTC(), ForceStopRevision: 1}}
		if _, err := s.l.Begin(t.Context(), record.ID, kind, string(record.State), "fixture", record); err != nil {
			t.Fatal(err)
		}
		if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
			_, err := tx.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
				INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at)
				SELECT 'other-'||n,'attempt','bound',1,1,json_object('id','other-'||n,'task_id','other-task','state','bound'),'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z' FROM seq`, size)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		allocations := testing.AllocsPerRun(5, func() {
			if err := s.l.Read(t.Context(), func(tx *ledger.ReadTx) error {
				found, err := originalRecoverySourcesTx(tx, "selected-episode")
				if err != nil || len(found) != 1 {
					t.Fatalf("indexed lookup: %+v %v", found, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
		if size == 24 {
			baseline = allocations
		} else if allocations > baseline*1.5 {
			t.Fatalf("original-source lookup grew with unrelated history: %.0f -> %.0f", baseline, allocations)
		}
		if err := func() error {
			var id, parent, unused int
			var detail string
			err := diagnostic.QueryRow(`EXPLAIN QUERY PLAN SELECT id FROM operations INDEXED BY `+originalRecoveryIndex+` WHERE kind='attempt' AND `+originalRecoveryKey+`=? AND id>? ORDER BY id LIMIT 1`, "selected-episode", "").Scan(&id, &parent, &unused, &detail)
			if err != nil {
				return err
			}
			if !strings.Contains(detail, "SEARCH operations USING INDEX "+originalRecoveryIndex) {
				t.Fatalf("original-source lookup scanned history: %s", detail)
			}
			t.Logf("history=%d allocations=%.0f query=%s", size, allocations, detail)
			return nil
		}(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOriginalRecoveryIndexRetainsCanonicalABTimeIdentity(t *testing.T) {
	_, clock := newService(t)
	r := Record{Spec: Spec{ID: "source", TaskID: "1"}, State: Failed, Abandoned: &Abandoned{At: clock.t, ForceStopRevision: 7, WorkspaceRecoveryID: "episode"}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeIdentityRecord(ledger.Operation{ID: r.ID, Data: raw})
	if err != nil || !decoded.Abandoned.At.Equal(r.Abandoned.At) || decoded.Abandoned.ForceStopRevision != 7 {
		t.Fatalf("time serialization changed original decision: %+v %v", decoded, err)
	}
}
