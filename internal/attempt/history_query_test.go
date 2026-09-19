package attempt

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func putHistoryAttempt(t testing.TB, book *ledger.Ledger, r Record) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, r.ID, string(r.State), string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestNativeHistoryPagesPreserveCompletionOrderAndScope(t *testing.T) {
	s, _ := newService(t)
	for _, id := range []string{"old", "new", "empty"} {
		if err := s.l.PutBinding(t.Context(), "task", id, map[string]any{"id": id, "transport": "console", "channel": "opaque"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.PutBinding(t.Context(), "task", "foreign", map[string]any{"id": "foreign", "transport": "feishu", "channel": "opaque"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Record{
		{Spec: Spec{ID: "old-new-result", TaskID: "old"}, State: Bound, StartedAt: time.Unix(8, 0), EndedAt: time.Unix(10, 0)},
		{Spec: Spec{ID: "new-previous-result", TaskID: "new"}, State: Bound, StartedAt: time.Unix(9, 0), EndedAt: time.Unix(9, 0)},
		{Spec: Spec{ID: "z-tie", TaskID: "old"}, State: Bound, StartedAt: time.Unix(9, 0)},
		{Spec: Spec{ID: "a-tie", TaskID: "new"}, State: Bound, StartedAt: time.Unix(9, 0)},
		{Spec: Spec{ID: "outside", TaskID: "foreign"}, State: Bound, StartedAt: time.Unix(11, 0)},
	} {
		putHistoryAttempt(t, s.l, r)
	}
	for _, tc := range []struct {
		query HistoryQuery
		want  []string
	}{
		{HistoryQuery{Conversation: "opaque", Limit: 1}, []string{"old-new-result", "z-tie", "new-previous-result", "a-tie"}},
		{HistoryQuery{TaskID: "old", Limit: 1}, []string{"old-new-result", "z-tie"}},
	} {
		q := tc.query
		ids := []string{}
		for {
			page, err := s.QueryHistory(t.Context(), q)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page: %+v %v", page, err)
			}
			same, err := s.QueryHistory(t.Context(), q)
			if err != nil || !reflect.DeepEqual(page, same) {
				t.Fatal("repeated page changed", err)
			}
			ids = append(ids, page.Items[0].ID)
			if len(ids) > len(tc.want) {
				t.Fatal("pagination loop")
			}
			if page.NextCursor == "" {
				break
			}
			q.Cursor = page.NextCursor
		}
		if !reflect.DeepEqual(ids, tc.want) {
			t.Fatalf("history order or coverage: %v want %v", ids, tc.want)
		}
	}
	for _, q := range []HistoryQuery{{TaskID: "missing"}, {Conversation: "missing"}, {TaskID: "empty"}} {
		page, err := s.QueryHistory(t.Context(), q)
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("empty page: %+v %v", page, err)
		}
	}
	first, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "old", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []HistoryQuery{
		{}, {TaskID: "old", Conversation: "opaque"}, {TaskID: "old", Limit: 101},
		{TaskID: "old", Limit: -1}, {TaskID: "old", Cursor: "12"},
		{TaskID: "new", Cursor: first.NextCursor}, {Conversation: "opaque", Cursor: first.NextCursor},
	} {
		if _, err := s.QueryHistory(t.Context(), q); !errors.Is(err, ErrInvalidHistoryQuery) {
			t.Fatalf("invalid query accepted: %+v %v", q, err)
		}
	}
	putHistoryAttempt(t, s.l, Record{Spec: Spec{ID: "backdated", TaskID: "old"}, State: Bound, StartedAt: time.Unix(1, 0)})
	if _, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "old", Cursor: first.NextCursor}); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed traversal did not expire", err)
	}
}

func TestNativeHistoryIndexesDoNotHideCorruptRows(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{`, `{"id":"bad","task_id":8}`, `{"id":"bad","usage":{"input":"no"}}`, `{"id":"wrong"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s, _ := newService(t)
			if _, err := s.l.DB().Exec(`INSERT INTO operations VALUES('bad','attempt','bound',1,1,?,'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "missing"}); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatal("corruption became a complete empty page", err)
			}
		})
	}
}

func TestNativeTaskHistoryUsesIndexedBoundedSeeks(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(book)
	diagnostic, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	for _, scope := range []string{"selected", "unrelated"} {
		_, err := s.l.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 10000)
			INSERT INTO operations SELECT ?||n,'attempt','bound',1,1,json_object('id',?||n,'task_id',?,'started_at','2026-09-19T00:00:00Z'),'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z' FROM seq`, scope, scope, scope)
		if err != nil {
			t.Fatal(err)
		}
	}
	q := HistoryQuery{TaskID: "selected", Limit: 2}
	page, err := s.QueryHistory(t.Context(), q)
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page: %+v %v", page, err)
	}
	for _, boundary := range []string{"", historyAtTie, historyBeforeTime} {
		args := []any{"selected"}
		switch boundary {
		case historyAtTie:
			args = append(args, nativeHistoryTime(time.Unix(9, 0)), "selected9")
		case historyBeforeTime:
			args = append(args, nativeHistoryTime(time.Unix(9, 0)))
		}
		args = append(args, 3)
		if err := func() error {
			rows, err := diagnostic.Query("EXPLAIN QUERY PLAN "+nativeHistorySQL(boundary), args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var details string
			for rows.Next() {
				var a, b, c int
				var detail string
				if err := rows.Scan(&a, &b, &c, &detail); err != nil {
					return err
				}
				details += detail
			}
			if !strings.Contains(details, "SEARCH operations USING INDEX operations_task_history") || strings.Contains(details, "TEMP B-TREE") {
				return fmt.Errorf("history seek scans/sorts: %s", details)
			}
			return rows.Err()
		}(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeHistoryScopeAndRowsUseOneReadSnapshot(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	putHistoryAttempt(t, book, Record{Spec: Spec{ID: "first", TaskID: "old"}, State: Bound, StartedAt: time.Unix(1, 0)})
	if err := book.PutBinding(t.Context(), "task", "old", map[string]any{"id": "old", "transport": "console", "channel": "opaque"}); err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		first, err := QueryHistoryTx(tx, HistoryQuery{Conversation: "opaque"})
		if err != nil || len(first.Items) != 1 {
			t.Fatalf("first: %+v %v", first, err)
		}
		raw, _ := json.Marshal(Record{Spec: Spec{ID: "second", TaskID: "old"}, State: Bound, StartedAt: time.Unix(2, 0)})
		if _, err := writer.Exec(`INSERT INTO operations VALUES('second','attempt','bound',1,1,?,'2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, string(raw)); err != nil {
			return err
		}
		if _, err := writer.Exec(`UPDATE bindings SET data='{"id":"old","transport":"console","channel":"moved"}' WHERE kind='task' AND id='old'`); err != nil {
			return err
		}
		again, err := QueryHistoryTx(tx, HistoryQuery{Conversation: "opaque"})
		if err != nil || !reflect.DeepEqual(first, again) {
			t.Fatalf("mixed scope/row snapshots: %+v %v", again, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := New(book).QueryHistory(t.Context(), HistoryQuery{Conversation: "opaque"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("old scope survived new boundary: %+v %v", page, err)
	}
}

func TestNativeHistoryReadIndexSurvivesReplicaRestore(t *testing.T) {
	source, _ := newService(t)
	putHistoryAttempt(t, source.l, Record{Spec: Spec{ID: "one", TaskID: "task"}, State: Bound, StartedAt: time.Unix(1, 0)})
	if _, err := source.l.DB().Exec(`DROP INDEX operations_task_history`); err != nil {
		t.Fatal(err)
	}
	raw, err := source.l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(raw); err != nil {
		t.Fatal(err)
	}
	page, err := New(target).QueryHistory(t.Context(), HistoryQuery{TaskID: "task"})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "one" {
		t.Fatalf("restored derived read index: %+v %v", page, err)
	}
}

func TestNativeHistoryRejectsCorruptLedgerEnvelope(t *testing.T) {
	for _, column := range []string{"created_at", "updated_at", "revision", "incarnation"} {
		t.Run(column, func(t *testing.T) {
			s, _ := newService(t)
			putHistoryAttempt(t, s.l, Record{Spec: Spec{ID: "valid", TaskID: "selected"}, State: Bound, StartedAt: time.Unix(1, 0)})
			putHistoryAttempt(t, s.l, Record{Spec: Spec{ID: "corrupt", TaskID: "unrelated"}, State: Bound, StartedAt: time.Unix(1, 0)})
			if _, err := s.l.DB().Exec(`UPDATE operations SET ` + column + `='broken' WHERE id='corrupt'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Limit: 1}); err == nil {
				t.Fatal("healthy history page hid corrupt ledger envelope")
			}
		})
	}
}
