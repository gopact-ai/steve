package ledger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func importFixture() (TransferFacts, map[string]json.RawMessage) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	return TransferFacts{
		Operations: []Operation{{ID: "op", Kind: "attempt", State: "bound", Revision: 2, Incarnation: 7, Data: json.RawMessage(`{"result":1}`), CreatedAt: at, UpdatedAt: at}},
		Events:     []Event{{OperationID: "op", Revision: 2, Incarnation: 7, From: "running", To: "bound", Actor: "import", At: at}},
		Names:      []NamedRef{{Name: "result", Version: 3, Artifact: "sha", UpdatedAt: at}},
		Bindings:   map[string]map[string]json.RawMessage{"task": {"task": json.RawMessage(`{"id":"task"}`)}},
	}, map[string]json.RawMessage{"projects": json.RawMessage(`{"p":1}`)}
}

func importCounts(t *testing.T, l *Ledger) []int {
	t.Helper()
	var counts []int
	for _, table := range []string{"operations", "events", "names", "bindings"} {
		var count int
		if err := l.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, count)
	}
	return counts
}

func TestImportFactsValidationCommitAndReplay(t *testing.T) {
	l := open(t, t.TempDir(), &clock{t: time.Now()})
	f, docs := importFixture()
	if err := l.ValidateImport(t.Context(), f, docs, nil); err != nil {
		t.Fatal(err)
	}
	if got := importCounts(t, l); got[0]+got[1]+got[2]+got[3] != 0 {
		t.Fatalf("validation wrote rows: %v", got)
	}
	for range 2 {
		if err := l.ImportFacts(t.Context(), f, docs, nil); err != nil {
			t.Fatal(err)
		}
		got, err := l.ExportOperations(t.Context(), []string{"op"})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(f.Operations)
		actual, _ := json.Marshal(got.Operations)
		if string(actual) != string(want) {
			t.Fatalf("operation: %s, want %s", actual, want)
		}
		if len(got.Events) != 1 {
			t.Fatalf("events: %+v", got.Events)
		}
		event := got.Events[0]
		if event.Seq != 1 || event.OperationID != "op" || event.Revision != 2 || event.Incarnation != 7 || event.From != "running" || event.To != "bound" || event.Actor != "import" || len(event.Fencings) != 0 || string(event.Effects) != "null" || !event.At.Equal(f.Events[0].At) {
			t.Fatalf("event changed: %+v", event)
		}
		name, ok, err := l.Name(t.Context(), "result")
		if err != nil || !ok || name != f.Names[0] {
			t.Fatalf("name: %+v %v", name, err)
		}
		if err := l.Update(t.Context(), func(tx *Tx) error {
			raw, found, err := tx.LoadDocument("projects")
			if err != nil || !found || string(raw) != string(docs["projects"]) {
				t.Fatalf("document: %s %v %v", raw, found, err)
			}
			bindings, err := tx.Bindings("task")
			if err != nil || string(bindings["task"]) != string(f.Bindings["task"]["task"]) {
				t.Fatalf("bindings: %s %v", bindings, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		counts := importCounts(t, l)
		if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 || counts[3] != 2 {
			t.Fatalf("replay changed row counts: %v", counts)
		}
	}
}

// A failure at the last write must roll back the earlier domains too.
func TestImportFactsRollsBackFinalDocumentFailure(t *testing.T) {
	l := open(t, t.TempDir(), &clock{t: time.Now()})
	f, docs := importFixture()
	if err := l.Update(t.Context(), func(tx *Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_document BEFORE INSERT ON bindings WHEN NEW.kind = '" + documentKind + "' BEGIN SELECT RAISE(ABORT, 'document rejected'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := l.ImportFacts(t.Context(), f, docs, nil)
	if err == nil || !strings.Contains(err.Error(), "document rejected") {
		t.Fatalf("error: %v", err)
	}
	if got := importCounts(t, l); got[0]+got[1]+got[2]+got[3] != 0 {
		t.Fatalf("partial commit: %v", got)
	}
}

func TestImportFactsConflictPrecedence(t *testing.T) {
	l := open(t, t.TempDir(), &clock{t: time.Now()})
	f, docs := importFixture()
	if err := l.ImportFacts(t.Context(), f, docs, nil); err != nil {
		t.Fatal(err)
	}
	f.Operations[0].State = "different"
	f.Names[0].Artifact = "different"
	f.Bindings["task"]["task"] = json.RawMessage(`{"different":true}`)
	docs["projects"] = json.RawMessage(`{"p":2}`)
	for _, want := range []string{"operation ID collision op", "name collision result", "binding collision task/task", "document changed during import: projects"} {
		for _, validate := range []bool{true, false} {
			err := l.Update(t.Context(), func(tx *Tx) error { return importFactsTx(tx, f, docs, nil, validate) })
			if err == nil || err.Error() != want {
				t.Fatalf("validate=%v: %v, want %s", validate, err, want)
			}
		}
		switch want {
		case "operation ID collision op":
			f.Operations = nil
		case "name collision result":
			f.Names = nil
		case "binding collision task/task":
			f.Bindings = nil
		}
	}
	// Even a caller's failure after a successful import rolls the whole transaction back.
	fresh := open(t, t.TempDir(), &clock{t: time.Now()})
	f, docs = importFixture()
	stop := errors.New("caller rollback")
	err := fresh.Update(t.Context(), func(tx *Tx) error {
		if err := importFactsTx(tx, f, docs, nil, false); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if got := importCounts(t, fresh); got[0]+got[1]+got[2]+got[3] != 0 {
		t.Fatalf("escaped caller transaction: %v", got)
	}
}
