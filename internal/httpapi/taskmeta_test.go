package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type fakeTaskMetaAdmin struct {
	consoleapi.Admin
	set func(context.Context, string, consoleapi.TaskMetaPatch) error
}

func (a fakeTaskMetaAdmin) SetTaskMeta(ctx context.Context, id string, patch consoleapi.TaskMetaPatch) error {
	return a.set(ctx, id, patch)
}

type taskMetaProjection struct {
	*readmodel.Model
	tasks []readmodel.Task
}

func (p taskMetaProjection) Snapshot(ctx context.Context) readmodel.Snapshot {
	snapshot := p.Model.Snapshot(ctx)
	snapshot.Tasks = p.tasks
	return snapshot
}

func taskMetaServer(t *testing.T, admin consoleapi.Admin, tasks ...readmodel.Task) *Server {
	t.Helper()
	server, err := NewServer(taskMetaProjection{Model: readmodel.New(readmodel.Sources{}), tasks: tasks}, ServerConfig{Addr: "127.0.0.1:0", Token: "owner"})
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
		patch consoleapi.TaskMetaPatch
	}
	calls := make(chan call, 1)
	want := readmodel.Task{ID: "42", Goal: "original goal", Title: "Release", Priority: "high", Labels: []string{"ui"}, ArchivedAt: "2026-09-05T12:00:00Z", State: "running", Lifecycle: "active", Execution: "running", Lane: "running"}
	server := taskMetaServer(t, fakeTaskMetaAdmin{set: func(_ context.Context, id string, patch consoleapi.TaskMetaPatch) error {
		calls <- call{id, patch}
		return nil
	}}, want)
	title, priority, labels, archived := "Release", "high", []string{"ui"}, true
	for _, tc := range []struct {
		name  string
		body  string
		patch consoleapi.TaskMetaPatch
	}{
		{"all fields", `{"title":"Release","priority":"high","labels":["ui"],"archived":true}`, consoleapi.TaskMetaPatch{Title: &title, Priority: &priority, Labels: &labels, Archived: &archived}},
		{"empty patch", `{}`, consoleapi.TaskMetaPatch{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, headers, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta", "owner", tc.body)
			if code != http.StatusOK || headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %v %s", code, headers, raw)
			}
			var got readmodel.Task
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
			server := taskMetaServer(t, fakeTaskMetaAdmin{set: func(context.Context, string, consoleapi.TaskMetaPatch) error {
				calls <- struct{}{}
				return nil
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
	server = taskMetaServer(t, fakeTaskMetaAdmin{set: func(context.Context, string, consoleapi.TaskMetaPatch) error {
		return errors.New("task 42 not found")
	}})
	code, headers, raw := taskMetaRequest(t, server, http.MethodPatch, "/console/tasks/42/meta", "owner", `{}`)
	if code != http.StatusBadRequest || headers.Get("Cache-Control") != "no-store" || !strings.Contains(string(raw), "task 42 not found") {
		t.Fatalf("admin error = %d %v %s", code, headers, raw)
	}
}
