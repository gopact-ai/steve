package readmodel

import (
	"fmt"
	"testing"
	"time"

	"errors"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"strings"
)

func completionLandingModel(t *testing.T) (*Model, *ledger.Ledger, *artifact.Store) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	old := time.Unix(100, 0).UTC()
	in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{
		"root":       {ID: "root", ProjectID: "p", State: task.StateRunning, Channel: "chat", UpdatedAt: old.Add(time.Hour)},
		"child":      {ID: "child", Parent: "root", Origin: "delegate:root", ProjectID: "p", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}, UpdatedAt: old},
		"grandchild": {ID: "grandchild", Parent: "child", Origin: "delegate:child", ProjectID: "p", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliverySuppressed}, UpdatedAt: old},
		"other":      {ID: "other", ProjectID: "p", State: task.StateRunning, Channel: "other-chat", UpdatedAt: old.Add(time.Hour)},
	}}
	for i := range 25 {
		id := fmt.Sprintf("recent-%02d", i)
		in.Tasks[id] = &task.Task{ID: id, Parent: "root", ProjectID: "p", State: task.StateCancelled, UpdatedAt: old.Add(time.Duration(i+1) * time.Second)}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	artifacts := artifact.New(t.TempDir(), book, projects, nil)
	f := newSourceFixture()
	f.disclosures, f.effects = nil, nil
	adapter := f.adapter()
	adapter.Book, adapter.Artifacts = book, artifacts
	return New(Sources{Tasks: tasks, Ledger: adapter}), book, artifacts
}

func assertCompletionLandingViews(t *testing.T, m *Model, want bool) {
	t.Helper()
	detail, err := m.TaskDetail(t.Context(), "root")
	if err != nil || detail.Task.CanComplete != want {
		t.Fatalf("task detail complete=%v err=%v, want %v", detail.Task.CanComplete, err, want)
	}
	if len(detail.Children.Items) != 20 || detail.Children.Total != 26 {
		t.Fatalf("fixture lost bounded child page: %d/%d", len(detail.Children.Items), detail.Children.Total)
	}
	page, err := m.TaskHistory(t.Context(), task.Query{Scope: task.Scope{Kind: "children"}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(items []Task, label string) {
		t.Helper()
		var root bool
		for _, item := range items {
			if item.ID == "root" {
				root = true
				if item.CanComplete != want {
					t.Errorf("%s root complete=%v, want %v", label, item.CanComplete, want)
				}
			}
			if item.ID == "other" && !item.CanComplete {
				t.Errorf("%s unrelated root was blocked", label)
			}
		}
		if !root {
			t.Errorf("%s omitted root", label)
		}
	}
	check(page.Items, "history")
	check(m.Snapshot(t.Context()).Tasks, "snapshot")
}

func TestCompletionViewsWaitForHiddenGrandchildQueuedArtifact(t *testing.T) {
	m, book, _ := completionLandingModel(t)
	assertCompletionLandingViews(t, m, true)
	pending := artifact.Pending{Project: "p", Artifact: "grandchild-result", Source: &artifact.Source{
		AttemptID: "grandchild-attempt", Execution: &task.ExecutionToken{TaskID: "grandchild", Epoch: 1},
	}}
	if err := book.PutBinding(t.Context(), "pending-landing", "p/grandchild-result", pending); err != nil {
		t.Fatal(err)
	}
	assertCompletionLandingViews(t, m, false)
	if err := book.DeleteBinding(t.Context(), "pending-landing", "p/grandchild-result"); err != nil {
		t.Fatal(err)
	}
	land := artifact.Landing{ID: "accepted", Project: "p", Artifact: pending.Artifact, Source: pending.Source, State: artifact.LandCommitted}
	if _, err := book.Begin(t.Context(), land.ID, "landing", land.State, "test", land); err != nil {
		t.Fatal(err)
	}
	assertCompletionLandingViews(t, m, true)
}

func TestCompletionViewsUseStandingLandingFactsNotRecentHistory(t *testing.T) {
	for _, scenario := range []string{"locked", "merge-conflict", "apply-conflict", "superseded", "different-epoch", "different-target", "write-lease", "wal", "by-hand", "unrelated", "already-removed-task"} {
		t.Run(scenario, func(t *testing.T) {
			m, book, artifacts := completionLandingModel(t)
			land := artifact.Landing{ID: "old", Project: "p", Artifact: "result", Target: project.Home{Path: "/canonical"},
				Source: &artifact.Source{AttemptID: "grandchild-attempt", Execution: &task.ExecutionToken{TaskID: "grandchild", Epoch: 1}},
				State:  artifact.LandLocked, StartedAt: time.Unix(100, 0).UTC()}
			want := false
			switch scenario {
			case "merge-conflict", "superseded", "different-epoch", "different-target", "write-lease", "wal":
				land.State, land.EndedAt = artifact.LandMergeConflicted, time.Unix(101, 0).UTC()
			case "apply-conflict":
				land.State = artifact.LandApplyConflicted
			case "by-hand":
				land.Source, want = nil, true
			case "unrelated":
				land.Source.Execution.TaskID, want = "other", true
			case "already-removed-task":
				land.Source.Execution.TaskID, want = "removed", true
			}
			if scenario == "write-lease" {
				land.Lease = &ledger.Lease{Key: "canonical:p"}
			}
			if scenario == "wal" {
				land.Round = 1
			}
			if _, err := book.Begin(t.Context(), land.ID, "landing", land.State, "test", land); err != nil {
				t.Fatal(err)
			}
			if scenario == "superseded" || scenario == "different-epoch" || scenario == "different-target" || scenario == "write-lease" || scenario == "wal" {
				committed := land
				committed.ID, committed.State = "accepted", artifact.LandCommitted
				if scenario == "different-epoch" {
					committed.Source = &artifact.Source{AttemptID: land.Source.AttemptID, Execution: &task.ExecutionToken{TaskID: "grandchild", Epoch: 2}}
				}
				if scenario == "different-target" {
					committed.Target.Path = "/another-canonical"
				}
				if _, err := book.Begin(t.Context(), committed.ID, "landing", committed.State, "test", committed); err != nil {
					t.Fatal(err)
				}
				want = scenario == "superseded"
			}
			for i := range 25 {
				other := artifact.Landing{ID: fmt.Sprintf("new-%02d", i), Project: "p", State: artifact.LandCommitted, EndedAt: time.Unix(200+int64(i), 0).UTC()}
				if _, err := book.Begin(t.Context(), other.ID, "landing", other.State, "test", other); err != nil {
					t.Fatal(err)
				}
			}
			recent, err := artifacts.RecentLandings(t.Context())
			if err != nil || len(recent) != 20 || recent[0].ID == "old" {
				t.Fatalf("fixture failed to hide the old landing: %+v, %v", recent, err)
			}
			// The root's gate is the point under test. An unrelated live root
			// really is blocked by its own landing in that one scenario.
			if scenario == "unrelated" {
				detail, err := m.TaskDetail(t.Context(), "root")
				if err != nil || !detail.Task.CanComplete {
					t.Fatalf("unrelated result blocked root: %+v, %v", detail.Task, err)
				}
				return
			}
			assertCompletionLandingViews(t, m, want)
		})
	}
}

func TestUnreadLandingGateIsNotKnownCompletionReadiness(t *testing.T) {
	m, _, _ := completionLandingModel(t)
	f := newSourceFixture()
	f.disclosures, f.effects = nil, nil
	f.fail["task-landings"] = errors.New("unread completion source")
	m.src.Ledger = f.adapter()
	snap := m.Snapshot(t.Context())
	for _, item := range snap.Tasks {
		if item.CanComplete {
			t.Errorf("task %s became ready with an unread gate", item.ID)
		}
	}
	if !strings.Contains(sourceHealth(t, snap, "ledger-task-landings").Error, "unread completion source") {
		t.Fatal("failed gate did not locate its source")
	}
	if _, err := m.TaskDetail(t.Context(), "root"); err == nil {
		t.Fatal("point read claimed complete readiness from an unread gate")
	}
}
