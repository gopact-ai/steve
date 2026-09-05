package task

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestMetaPersistsWithTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	created := mustCreate(t, store, "original goal", "console:main")
	title, priority, labels, archived, rank := "Release", "high", []string{"console", "release"}, true, 7
	meta, err := store.SetMeta(created.ID, MetaPatch{Title: &title, Priority: &priority, Labels: &labels, Archived: &archived, Rank: &rank})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := store.Get(created.ID)
	if !reflect.DeepEqual(after, created) {
		t.Fatalf("metadata changed the task: %+v", after)
	}
	want := meta.clone()
	// Callers own both inputs and results; changing them cannot rewrite the store.
	labels[0] = "input changed"
	meta.Labels[0] = "result changed"
	*meta.ArchivedAt = time.Time{}
	read := store.MetaOf(created.ID)
	read.Labels[0] = "read changed"
	*read.ArchivedAt = time.Time{}
	if !reflect.DeepEqual(store.MetaOf(created.ID), want) {
		t.Fatal("metadata aliases a caller's slice or timestamp")
	}
	if _, err := store.Advance(created.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, store, "another task", "console:main")
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.MetaOf(created.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened metadata = %+v, want %+v", got, want)
	}
	if got, ok := reopened.Get(created.ID); !ok || got.State != StateDone || got.Goal != created.Goal {
		t.Fatalf("reopened task = %+v, found %v", got, ok)
	}
}

func TestMetaReadsLegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(`{"next_id":2,"tasks":{"1":{"id":"1","goal":"legacy","state":"running"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "missing"} {
		if got := store.MetaOf(id); !reflect.DeepEqual(got, Meta{Priority: "normal"}) {
			t.Fatalf("default metadata for %s = %+v", id, got)
		}
	}
	title := "Legacy title"
	if _, err := store.SetMeta("1", MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.MetaOf("1"); got.Title != title || got.Priority != "normal" {
		t.Fatalf("legacy metadata after reopen = %+v", got)
	}
}

type metaDocument struct {
	ledger.Doc
	writes int
	fail   bool
}

func (d *metaDocument) Save(raw []byte) error {
	if d.fail {
		return errors.New("disk unavailable")
	}
	d.writes++
	return d.Doc.Save(raw)
}

func TestMetaPatchIsOptionalAndIdempotent(t *testing.T) {
	store, clock := newStore(t)
	created := mustCreate(t, store, "goal", "console:main")
	doc := &metaDocument{Doc: store.doc}
	store.doc = doc
	if _, err := store.SetMeta(created.ID, MetaPatch{}); err != nil || doc.writes != 0 {
		t.Fatalf("empty patch: writes=%d, err=%v", doc.writes, err)
	}
	title, priority, labels, archived, rank := "Title", "low", []string{"todo"}, true, 4
	patch := MetaPatch{Title: &title, Priority: &priority, Labels: &labels, Archived: &archived, Rank: &rank}
	first, err := store.SetMeta(created.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArchivedAt == nil || !first.ArchivedAt.Equal(*clock) {
		t.Fatalf("archive time = %v, want %v", first.ArchivedAt, *clock)
	}
	*clock = clock.Add(time.Hour)
	again, err := store.SetMeta(created.ID, patch)
	if err != nil || !reflect.DeepEqual(again, first) || doc.writes != 1 {
		t.Fatalf("retry changed metadata: %+v, writes=%d, err=%v", again, doc.writes, err)
	}
	title = "Renamed"
	renamed, err := store.SetMeta(created.ID, MetaPatch{Title: &title})
	first.Title = title
	if err != nil || !reflect.DeepEqual(renamed, first) {
		t.Fatalf("title patch changed omitted fields: %+v, err=%v", renamed, err)
	}
	title, priority, labels, archived, rank = "", "", []string{}, false, 0
	cleared, err := store.SetMeta(created.ID, patch)
	if err != nil || cleared.Title != "" || cleared.Priority != "normal" || len(cleared.Labels) != 0 || cleared.ArchivedAt != nil || cleared.Rank != 0 {
		t.Fatalf("clear metadata = %+v, err=%v", cleared, err)
	}
	writes := doc.writes
	priority = "normal"
	if _, err := store.SetMeta(created.ID, patch); err != nil || doc.writes != writes {
		t.Fatalf("normal priority retry: writes=%d, want %d, err=%v", doc.writes, writes, err)
	}
	archived = true
	rearchived, err := store.SetMeta(created.ID, MetaPatch{Archived: &archived})
	if err != nil || rearchived.ArchivedAt == nil || !rearchived.ArchivedAt.Equal(*clock) {
		t.Fatalf("rearchive = %+v, err=%v", rearchived, err)
	}
	if got, _ := store.Get(created.ID); !reflect.DeepEqual(got, created) {
		t.Fatalf("metadata changed task state or activity: %+v", got)
	}
}

func TestMetaRejectsMissingInvalidAndFailedWrites(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "goal", "console:main")
	title := "Saved"
	before, err := store.SetMeta(created.ID, MetaPatch{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	doc := &metaDocument{Doc: store.doc}
	store.doc = doc
	if _, err := store.SetMeta("missing", MetaPatch{Title: &title}); err == nil {
		t.Fatal("missing task accepted metadata")
	}
	title, invalid := "Unsaved", "urgent"
	if _, err := store.SetMeta(created.ID, MetaPatch{Title: &title, Priority: &invalid}); err == nil {
		t.Fatal("invalid priority accepted")
	}
	if doc.writes != 0 {
		t.Fatalf("invalid patches wrote %d times", doc.writes)
	}
	doc.fail = true
	if _, err := store.SetMeta(created.ID, MetaPatch{Title: &title}); err == nil {
		t.Fatal("failed persistence reported success")
	}
	if got := store.MetaOf(created.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed patch changed memory: %+v", got)
	}
}

func TestMetaNotifiesObserverAfterPersistence(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "goal", "console:main")
	changes := make(chan string, 8)
	store.SetObserver(func(id string) {
		// Reentering the store proves notification does not hold its lock.
		_ = store.MetaOf(id)
		changes <- id
	})
	title, priority, labels, archived, rank := "Title", "high", []string{"label"}, true, 2
	for _, patch := range []MetaPatch{{Title: &title}, {Priority: &priority}, {Labels: &labels}, {Archived: &archived}, {Rank: &rank}} {
		meta, err := store.SetMeta(created.ID, patch)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case id := <-changes:
			if id != created.ID {
				t.Fatalf("observer id = %q, want %q", id, created.ID)
			}
			reopened, err := openWith(store.doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.MetaOf(id); !got.equal(meta) {
				t.Fatalf("observer ran before persistence: %+v, want %+v", got, meta)
			}
		case <-time.After(time.Second):
			t.Fatal("metadata write did not notify observer")
		}
		if _, err := store.SetMeta(created.ID, patch); err != nil {
			t.Fatal(err)
		}
	}
	store.doc = &metaDocument{Doc: store.doc, fail: true}
	title = "Unsaved"
	if _, err := store.SetMeta(created.ID, MetaPatch{Title: &title}); err == nil {
		t.Fatal("failed persistence reported success")
	}
	select {
	case id := <-changes:
		t.Fatalf("no-op or failed write notified observer for %s", id)
	case <-time.After(30 * time.Millisecond):
	}
}
