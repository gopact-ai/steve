package readmodel

import (
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestSuppressedCompletionConsumersUseProductionOwnerSummary(t *testing.T) {
	for _, scenario := range []string{"suppressed", "cancelled-parent", "queued", "uncertain", "running", "missing-result", "parent-running", "direct-suppressed"} {
		t.Run(scenario, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{}}
			at := time.Unix(100, 0).UTC()
			in.Tasks["root"] = &task.Task{ID: "root", ProjectID: "p", Channel: "chat", State: task.StateRunning, UpdatedAt: at.Add(time.Hour)}
			in.Tasks["child"] = &task.Task{ID: "child", Parent: "root", Origin: "delegate:root", ProjectID: "p", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}, UpdatedAt: at}
			g := &task.Task{ID: "grandchild", Parent: "child", Origin: "delegate:child", ProjectID: "p", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliverySuppressed}, UpdatedAt: at}
			in.Tasks[g.ID] = g
			want := scenario == "suppressed" || scenario == "cancelled-parent"
			switch scenario {
			case "cancelled-parent":
				in.Tasks["child"].State = task.StateCancelled
			case "queued":
				g.Delivery.State = task.DeliveryQueued
			case "uncertain":
				g.Delivery.State = task.DeliveryUncertain
			case "running":
				g.State = task.StateRunning
			case "missing-result":
				g.Result = nil
			case "parent-running":
				in.Tasks["child"].State = task.StateRunning
			case "direct-suppressed":
				g.Parent, g.Origin = "root", "delegate:root"
			}
			// Keep the historical child/grandchild out of the recent workset
			// and root's bounded detail page. Neither UI page owns eligibility.
			for i := range 25 {
				id := fmt.Sprintf("recent-%02d", i)
				in.Tasks[id] = &task.Task{ID: id, Parent: "root", ProjectID: "p", State: task.StateCancelled, UpdatedAt: at.Add(time.Duration(i+1) * time.Second)}
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
				t.Fatal(err)
			}
			store, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			fixture := newSourceFixture()
			fixture.disclosures, fixture.effects = nil, nil
			m := New(Sources{Tasks: store, Ledger: fixture.adapter()})
			inspect := func(expected bool) {
				t.Helper()
				head, _ := store.Header("root")
				if head.Summary.CanComplete != expected {
					t.Errorf("owner Header = %v, want %v", head.Summary.CanComplete, expected)
				}
				detail, err := m.TaskDetail(t.Context(), "root")
				if err != nil || detail.Task.CanComplete != expected || detail.Children.Total < 26 || len(detail.Children.Items) != 20 {
					t.Fatalf("bounded detail can_complete = %v, children=%d/%d, err=%v; want %v", detail.Task.CanComplete, len(detail.Children.Items), detail.Children.Total, err, expected)
				}
				for _, h := range detail.Children.Items {
					child, _ := store.Header("child")
					if h.ID == "grandchild" || h.ID == "child" && child.UpdatedAt.Equal(at) {
						t.Fatal("fixture exposed the hidden historical child")
					}
				}
				page, err := m.TaskHistory(t.Context(), task.Query{Scope: task.Scope{Kind: "children"}, Limit: 1})
				if err != nil || len(page.Items) != 1 || page.Items[0].CanComplete != expected {
					t.Fatalf("history can_complete = %+v, %v; want %v", page, err, expected)
				}
				found := false
				for _, item := range m.Snapshot(t.Context()).Tasks {
					if item.ID == "root" {
						found = true
						if item.CanComplete != expected {
							t.Fatalf("workset snapshot can_complete = %v, want %v", item.CanComplete, expected)
						}
					}
					if scenario == "suppressed" && item.ID == "grandchild" {
						t.Fatal("legal historical suppression must not require loading the descendant")
					}
				}
				if !found {
					t.Fatal("snapshot omitted root")
				}
			}
			inspect(want)
			if scenario == "parent-running" {
				if _, err := store.Advance("child", task.StateDone); err != nil {
					t.Fatal(err)
				}
				inspect(true)
			}
		})
	}
}
