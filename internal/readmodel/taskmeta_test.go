package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

type fakeTaskMetaAdmin struct {
	Admin
	set func(context.Context, string, TaskMetaPatch) (Task, error)
}

func (a fakeTaskMetaAdmin) SetTaskMeta(ctx context.Context, id string, patch TaskMetaPatch) (Task, error) {
	return a.set(ctx, id, patch)
}

func taskMetaServer(t *testing.T, admin Admin) *Server {
	t.Helper()
	server, err := NewServer(New(Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetAdmin(admin)
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func taskMetaRequest(t *testing.T, server *Server, method, path, token, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, raw
}

func TestTaskMetaEndpointPassesOptionalFieldsAndReturnsProjection(t *testing.T) {
	type call struct {
		id    string
		patch TaskMetaPatch
	}
	calls := make(chan call, 1)
	want := Task{ID: "42", Goal: "original goal", Title: "Release", Priority: "high", Labels: []string{"ui"}, ArchivedAt: "2026-09-05T12:00:00Z", State: "running", Lifecycle: "active", Execution: "running", Lane: "running"}
	server := taskMetaServer(t, fakeTaskMetaAdmin{set: func(_ context.Context, id string, patch TaskMetaPatch) (Task, error) {
		calls <- call{id, patch}
		return want, nil
	}})
	title, priority, labels, archived := "Release", "high", []string{"ui"}, true
	for _, tc := range []struct {
		name  string
		body  string
		patch TaskMetaPatch
	}{
		{"all fields", `{"title":"Release","priority":"high","labels":["ui"],"archived":true}`, TaskMetaPatch{Title: &title, Priority: &priority, Labels: &labels, Archived: &archived}},
		{"empty patch", `{}`, TaskMetaPatch{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, headers, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta", "owner", tc.body)
			if code != http.StatusOK || headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %v %s", code, headers, raw)
			}
			var got Task
			if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("projection = %+v, err=%v", got, err)
			}
			called := <-calls
			if called.id != "42" || !reflect.DeepEqual(called.patch, tc.patch) {
				t.Fatalf("admin call = %+v, want %+v", called, tc.patch)
			}
		})
	}
	code, _, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta?token=owner", "", `{"title":"","priority":"","labels":[],"archived":false}`)
	if code != http.StatusOK {
		t.Fatalf("clear response = %d %s", code, raw)
	}
	cleared := (<-calls).patch
	if cleared.Title == nil || *cleared.Title != "" || cleared.Priority == nil || *cleared.Priority != "" || cleared.Labels == nil || len(*cleared.Labels) != 0 || cleared.Archived == nil || *cleared.Archived {
		t.Fatalf("explicit zero values were lost: %+v", cleared)
	}
}

func TestTaskMetaEndpointIsGuardedAndRejectsInvalidBodies(t *testing.T) {
	for _, tc := range []struct {
		name, method, token, body string
		code                      int
	}{
		{"no token", http.MethodPatch, "", `{}`, http.StatusUnauthorized},
		{"wrong token", http.MethodPatch, "guest", `{}`, http.StatusUnauthorized},
		{"wrong method", http.MethodPost, "owner", `{}`, http.StatusMethodNotAllowed},
		{"empty body", http.MethodPatch, "owner", ``, http.StatusBadRequest},
		{"malformed", http.MethodPatch, "owner", `{`, http.StatusBadRequest},
		{"null", http.MethodPatch, "owner", `null`, http.StatusBadRequest},
		{"array", http.MethodPatch, "owner", `[]`, http.StatusBadRequest},
		{"wrong title type", http.MethodPatch, "owner", `{"title":7}`, http.StatusBadRequest},
		{"wrong labels type", http.MethodPatch, "owner", `{"labels":"ui"}`, http.StatusBadRequest},
		{"wrong archive type", http.MethodPatch, "owner", `{"archived":"true"}`, http.StatusBadRequest},
		{"state is not metadata", http.MethodPatch, "owner", `{"state":"done"}`, http.StatusBadRequest},
		{"rank is store only", http.MethodPatch, "owner", `{"rank":3}`, http.StatusBadRequest},
		{"trailing object", http.MethodPatch, "owner", `{} {}`, http.StatusBadRequest},
		{"trailing garbage", http.MethodPatch, "owner", `{} invalid`, http.StatusBadRequest},
		{"oversized", http.MethodPatch, "owner", `{"title":"` + strings.Repeat("a", 64<<10) + `"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := make(chan struct{}, 1)
			server := taskMetaServer(t, fakeTaskMetaAdmin{set: func(context.Context, string, TaskMetaPatch) (Task, error) {
				calls <- struct{}{}
				return Task{}, nil
			}})
			code, _, raw := taskMetaRequest(t, server, tc.method, "/console/tasks/42/meta", tc.token, tc.body)
			if code != tc.code {
				t.Fatalf("response = %d %s, want %d", code, raw, tc.code)
			}
			select {
			case <-calls:
				t.Fatal("rejected request reached admin")
			default:
			}
		})
	}
}

func TestTaskMetaEndpointReportsUnwiredAndAdminErrors(t *testing.T) {
	server := taskMetaServer(t, nil)
	if code, _, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta", "owner", `{}`); code != http.StatusNotImplemented {
		t.Fatalf("unwired response = %d %s", code, raw)
	}
	server = taskMetaServer(t, fakeTaskMetaAdmin{set: func(context.Context, string, TaskMetaPatch) (Task, error) {
		return Task{}, errors.New("task 42 not found")
	}})
	code, headers, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta", "owner", `{}`)
	if code != http.StatusBadRequest || headers.Get("Cache-Control") != "no-store" || !strings.Contains(string(raw), "task 42 not found") {
		t.Fatalf("admin error = %d %v %s", code, headers, raw)
	}
}

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
