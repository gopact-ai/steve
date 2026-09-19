package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

func TestWorkPagesAndHistoricalDetailHTTPContract(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{"root": {ID: "root", State: task.StateRunning, ProjectID: "p", Transport: "console", Channel: "opaque"}}}
	for i := range 30 {
		id := fmt.Sprintf("closed-%02d", i)
		in.Tasks[id] = &task.Task{ID: id, Parent: "root", ProjectID: "p", Transport: "console", Channel: "opaque", State: task.StateDone, UpdatedAt: time.Unix(int64(i), 0), Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}}
		in.Tasks["root"].Attempts = append(in.Tasks["root"].Attempts, task.Attempt{StartedAt: time.Unix(int64(i), 0), EndedAt: time.Unix(int64(i)+1, 0)})
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		r := attempt.Record{Spec: attempt.Spec{ID: fmt.Sprint("native-", i), TaskID: "root"}, StartedAt: time.Unix(int64(i), 0)}
		if _, err := book.Begin(t.Context(), r.ID, "attempt", string(attempt.Bound), "test", r); err != nil {
			t.Fatal(err)
		}
	}
	s := serve(t, readmodel.New(readmodel.Sources{Tasks: tasks}), ServerConfig{Token: "owner"})
	s.SetAdmin(&admin.Service{Tasks: tasks, Attempts: attempt.New(book)})
	get := func(path string, into any) {
		t.Helper()
		code, headers, raw := taskMetaRequest(t, s, http.MethodGet, path, "owner", "")
		if code != http.StatusOK || (path != "/state" && headers.Get("Cache-Control") != "no-store") {
			t.Fatalf("%s: %d %s", path, code, raw)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	var snap readmodel.Snapshot
	get("/state", &snap)
	if len(snap.Tasks) != 21 || snap.TaskCoverage.Total != 31 || !snap.TaskCoverage.HasMoreClosed {
		t.Fatalf("base state coverage: %d %+v", len(snap.Tasks), snap.TaskCoverage)
	}
	var detail readmodel.TaskDetail
	get("/console/tasks/closed-00", &detail)
	if detail.Task.ID != "closed-00" || detail.Children.Total != 0 {
		t.Fatalf("historical detail not independently reachable: %+v", detail)
	}
	get("/console/tasks/root", &detail)
	if detail.Children.Total != 30 || len(detail.Children.Items) != 20 || detail.Children.NextCursor == "" ||
		detail.Accounting.Total != 30 || len(detail.Accounting.Items) != 20 || detail.Accounting.NextCursor == "" {
		t.Fatalf("detail history not bounded/pageable: %+v", detail)
	}
	var page readmodel.TaskPage
	get("/console/tasks?scope=children&scope_id=root&limit=1", &page)
	if len(page.Items) != 1 || page.Total != 30 || page.NextCursor == "" {
		t.Fatal("task page", page)
	}
	taskCursor := page.NextCursor
	firstID := page.Items[0].ID
	get("/console/tasks?scope=children&scope_id=root&limit=1&cursor="+url.QueryEscape(taskCursor), &page)
	if len(page.Items) != 1 || page.Items[0].ID == firstID {
		t.Fatal("task pagination lost rows", page)
	}
	var accounting readmodel.AccountingPage
	get("/console/tasks/root/accounting?limit=1", &accounting)
	if accounting.Total != 30 || len(accounting.Items) != 1 || accounting.Items[0].Index != 29 {
		t.Fatal("accounting cursor/index contract", accounting)
	}
	var native consoleapi.AttemptHistoryPage
	get("/console/attempts?conversation=opaque&limit=1", &native)
	if len(native.Items) != 1 || native.Items[0].ID != "native-1" || native.NextCursor == "" {
		t.Fatal("native attempts missing", native)
	}
	get("/console/tasks/root/attempts?limit=1", &native)
	if len(native.Items) != 1 || native.Items[0].ID != "native-1" {
		t.Fatal("task native endpoint is not a bounded page", native)
	}
	for _, path := range []string{"/console/tasks?limit=101", "/console/tasks?cursor=old", "/console/attempts?conversation=opaque&limit=101", "/console/tasks/root/accounting?limit=-1"} {
		if code, _, raw := taskMetaRequest(t, s, http.MethodGet, path, "owner", ""); code != http.StatusBadRequest {
			t.Fatalf("invalid query accepted: %s %d %s", path, code, raw)
		}
	}
	title := "changed"
	if _, err := tasks.SetMeta("closed-00", task.MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := taskMetaRequest(t, s, http.MethodGet, "/console/tasks?scope=children&scope_id=root&cursor="+url.QueryEscape(taskCursor), "owner", ""); code != http.StatusConflict {
		t.Fatalf("stale cursor is not explicit: %d %s", code, raw)
	}
	for _, path := range []string{"/console/tasks", "/console/tasks/closed-00", "/console/tasks/root/accounting", "/console/plans", "/console/attempts?conversation=opaque"} {
		if code, _, _ := taskMetaRequest(t, s, http.MethodGet, path, "", ""); code != http.StatusUnauthorized {
			t.Fatal("work read bypasses owner authorization", path, code)
		}
	}
	if _, err := book.DB().Exec(`UPDATE operations SET created_at='bad-envelope' WHERE id='native-0'`); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := taskMetaRequest(t, s, http.MethodGet, "/console/attempts?task_id=root", "owner", ""); code != http.StatusInternalServerError {
		t.Fatalf("corrupt ledger envelope hidden: %d %s", code, raw)
	}
	broken := serve(t, readmodel.New(readmodel.Sources{Tasks: tasks, Ledger: readmodel.Ledger{}}), ServerConfig{Token: "owner"})
	broken.SetAdmin(&admin.Service{Tasks: tasks})
	for _, path := range []string{"/console/tasks/closed-00", "/console/tasks?limit=1"} {
		if code, _, raw := taskMetaRequest(t, broken, http.MethodGet, path, "owner", ""); code != http.StatusInternalServerError {
			t.Fatalf("projection source error hidden %s: %d %s", path, code, raw)
		}
	}
}
