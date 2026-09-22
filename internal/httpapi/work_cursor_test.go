package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

// Unrelated live work must not prevent paging a stable historical scope.
func TestWorkPagesContinueAfterUnrelatedOwnerMutation(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		planWrite  bool
	}{
		{"closed-task-accounting", "/console/tasks/closed-root/accounting?", false},
		{"children", "/console/tasks?scope=children&scope_id=closed-root&", false},
		{"project-tasks", "/console/tasks?scope=project&scope_id=selected&", false},
		{"project-plans", "/console/plans?project_id=selected&", true},
		{"task-plans", "/console/plans?task_id=closed-root&", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			in := task.ProjectTransfer{Project: "selected", Tasks: map[string]*task.Task{
				"closed-root": {ID: "closed-root", State: task.StateDone, ProjectID: "selected",
					Attempts: []task.Attempt{
						{StartedAt: time.Unix(1, 0), EndedAt: time.Unix(2, 0)},
						{StartedAt: time.Unix(3, 0), EndedAt: time.Unix(4, 0)},
					}},
				"closed-child-a": {ID: "closed-child-a", Parent: "closed-root", State: task.StateDone, ProjectID: "selected"},
				"closed-child-b": {ID: "closed-child-b", Parent: "closed-root", State: task.StateDone, ProjectID: "selected"},
			}}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
				t.Fatal(err)
			}
			tasks, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			other, err := tasks.Create(task.Task{Transport: "console", Channel: "other", ProjectID: "unrelated"})
			if err != nil {
				t.Fatal(err)
			}
			plans, err := plan.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Step{ID: "step", Goal: "fixture", Agent: "test", State: plan.StepDone, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "isolated fixture"}}
			for range 2 {
				if _, err := plans.Create(plan.Plan{TaskID: "closed-root", ProjectID: "selected", Steps: []plan.Step{step}}); err != nil {
					t.Fatal(err)
				}
			}
			otherPlan, err := plans.Create(plan.Plan{TaskID: other.ID, ProjectID: "unrelated", Steps: []plan.Step{step}})
			if err != nil {
				t.Fatal(err)
			}
			server := serve(t, readmodel.New(readmodel.Sources{Tasks: tasks, Plans: plans}), ServerConfig{Token: "owner"})
			server.SetAdmin(&admin.Service{Tasks: tasks})
			read := func(path string) map[string]any {
				t.Helper()
				code, _, raw := taskMetaRequest(t, server, http.MethodGet, path, "owner", "")
				if code != http.StatusOK {
					t.Fatalf("read HTTP %d: %s", code, raw)
				}
				var page map[string]any
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				return page
			}
			allBefore := read(tc.path + "limit=100")
			for i := range 3 {
				first := read(tc.path + "limit=1")
				cursor, ok := first["next_cursor"].(string)
				if !ok || cursor == "" {
					t.Fatal("fixture must have multiple pages")
				}
				if tc.planWrite {
					step.Goal = fmt.Sprintf("unrelated revision %d", i+1)
					if _, err := plans.Revise(otherPlan.ID, []plan.Step{step}, "fixture", "unrelated owner revision"); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := tasks.Begin(other.ID, "fixture", "", ""); err != nil {
						t.Fatal(err)
					}
					if _, err := tasks.Finish(other.ID, task.OutcomeOK, task.Tokens{Input: 1, Total: 1}, 0); err != nil {
						t.Fatal(err)
					}
				}
				if after := read(tc.path + "limit=100"); !reflect.DeepEqual(allBefore, after) {
					t.Fatalf("selected result changed, invalid counterexample: before=%+v after=%+v", allBefore, after)
				}
				code, _, raw := taskMetaRequest(t, server, http.MethodGet, tc.path+"limit=1&cursor="+url.QueryEscape(cursor), "owner", "")
				if code != http.StatusOK {
					t.Fatalf("selected result unchanged after unrelated owner mutation, continuation HTTP %d, want %d: %s", code, http.StatusOK, raw)
				}
				t.Logf("round=%d selected result deep-equal; unrelated owner mutation; continuation HTTP%d", i+1, code)
			}
		})
	}
}
