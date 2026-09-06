package readmodel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskMetaSnapshotAndChangeNotification(t *testing.T) {
	store, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(task.Task{Goal: "original goal", Channel: "console:main"})
	if err != nil {
		t.Fatal(err)
	}
	model := New(Sources{Tasks: store})
	before := model.Snapshot(context.Background()).Tasks[0]
	if before.Priority != "normal" || before.Title != "" || len(before.Labels) != 0 || before.ArchivedAt != "" {
		t.Fatalf("default projection = %+v", before)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, stop := model.Subscribe(ctx)
	defer stop()
	store.SetObserver(model.TaskChanged)
	title, priority, labels, archived := "Release", "high", []string{"ui", "release"}, true
	meta, err := store.SetMeta(created.ID, task.MetaPatch{Title: &title, Priority: &priority, Labels: &labels, Archived: &archived})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Kind != "task.changed" || event.TaskID != created.ID || event.Conversation != created.Channel {
			t.Fatalf("change notification = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("metadata change did not invalidate the task")
	}
	got := model.Snapshot(ctx).Tasks[0]
	if got.Title != title || got.Priority != priority || !reflect.DeepEqual(got.Labels, labels) || got.ArchivedAt != meta.ArchivedAt.Format(time.RFC3339) {
		t.Fatalf("metadata projection = %+v", got)
	}
	got.Title, got.Priority, got.Labels, got.ArchivedAt = before.Title, before.Priority, before.Labels, before.ArchivedAt
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("metadata changed the execution projection: %+v, want %+v", got, before)
	}
	title, labels, archived = "", []string{}, false
	if _, err := store.SetMeta(created.ID, task.MetaPatch{Title: &title, Labels: &labels, Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(model.Snapshot(ctx).Tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"title", "labels", "archived_at"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("cleared field %s was not omitted: %s", key, raw)
		}
	}
}
