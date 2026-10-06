package readmodel

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func recordCompletionClose(t *testing.T, store *state.Store, taskID, name string) state.OwedClose {
	t.Helper()
	return recordCompletionCloseOn(t, store, taskID, name, "node")
}

func recordCompletionCloseOn(t *testing.T, store *state.Store, taskID, name, node string) state.OwedClose {
	t.Helper()
	session := state.Session{ConversationID: "close-" + name, AgentID: "agent", NodeID: node, HarnessID: "harness", UpstreamID: "session-" + name}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	owed := state.OwedClose{NodeID: session.NodeID, HarnessID: session.HarnessID, UpstreamID: session.UpstreamID,
		NativeContext: "native-" + name, TaskID: taskID, AttemptID: "attempt-" + name, OwedAt: "2026-10-06T00:00:00Z"}
	if err := store.ArchiveSessionOwingClose(session.ConversationID, session.AgentID, owed.OwedAt, owed); err != nil {
		t.Fatal(err)
	}
	return owed
}

func assertCompletionCloseViews(t *testing.T, m *Model, root, other bool) {
	t.Helper()
	check := func(items []Task, label string) {
		t.Helper()
		for id, want := range map[string]bool{"root": root, "other": other} {
			found := false
			for _, item := range items {
				if item.ID == id {
					found = true
					if item.CanComplete != want {
						t.Errorf("%s task %s complete=%v, want %v", label, id, item.CanComplete, want)
					}
				}
			}
			if !found {
				t.Errorf("%s omitted task %s", label, id)
			}
		}
	}
	page, err := m.TaskHistory(t.Context(), task.Query{Scope: task.Scope{Kind: "children"}})
	if err != nil {
		t.Errorf("query: %v", err)
	} else {
		check(page.Items, "query")
	}
	var details []Task
	for _, id := range []string{"root", "other"} {
		detail, err := m.TaskDetail(t.Context(), id)
		if err != nil {
			t.Errorf("detail %s: %v", id, err)
			continue
		}
		details = append(details, detail.Task)
		if id == "root" && (len(detail.Children.Items) != 20 || detail.Children.Total != 26) {
			t.Fatal("fixture lost its bounded child page")
		}
	}
	if len(details) == 2 {
		check(details, "detail")
	}
	check(m.Snapshot(t.Context()).Tasks, "snapshot")
}

func TestCompletionViewsWaitForStandingOwedCloses(t *testing.T) {
	for _, taskID := range []string{"root", "child", "grandchild", "other", "removed"} {
		t.Run(taskID, func(t *testing.T) {
			m, book, _ := completionLandingModel(t)
			assertCompletionCloseViews(t, m, true, true)
			for _, item := range m.Snapshot(t.Context()).Tasks {
				if item.ID == "child" || item.ID == "grandchild" {
					t.Fatal("fixture did not hide the historical descendants")
				}
			}
			detail, err := m.TaskDetail(t.Context(), "root")
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range detail.Children.Items {
				if item.ID == "child" {
					t.Fatal("fixture did not hide the child beyond the first page")
				}
			}
			store, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			owed := recordCompletionClose(t, store, taskID, taskID)
			blocked := taskID == "root" || taskID == "child" || taskID == "grandchild"
			assertCompletionCloseViews(t, m, !blocked, taskID != "other")
			err = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				return state.CheckTaskDeletionTx(tx, []string{"root", "child", "grandchild"})
			})
			if errors.Is(err, state.ErrCloseOwed) != blocked || (!blocked && err != nil) {
				t.Fatalf("projection disagrees with the transactional guard: %v", err)
			}
			if err := store.SettleOwedClose(owed); err != nil {
				t.Fatal(err)
			}
			assertCompletionCloseViews(t, m, true, true)
		})
	}
}

func TestCompletionViewsRequireExactRefundOfEveryOwedClose(t *testing.T) {
	m, book, _ := completionLandingModel(t)
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	first := recordCompletionClose(t, store, "grandchild", "first")
	second := recordCompletionClose(t, store, "grandchild", "second")
	other := recordCompletionClose(t, store, "other", "other")
	assertCompletionCloseViews(t, m, false, false)
	stale := first
	stale.AttemptID = "previous-execution"
	if err := store.SettleOwedClose(stale); err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, false, false)
	if err := store.SettleOwedClose(first); err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, false, false)
	if err := store.SettleOwedClose(second); err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, true, false)
	if err := store.SettleOwedClose(other); err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, true, true)
}

func TestCompletionViewsOwedCancelledChildWithEndedAccounting(t *testing.T) {
	m, book, _ := completionLandingModelWithTasks(t, func(tasks map[string]*task.Task) {
		ended := task.Attempt{StartedAt: time.Unix(100, 0).UTC(), EndedAt: time.Unix(101, 0).UTC()}
		tasks["root"].Attempts, tasks["child"].Attempts = []task.Attempt{ended}, []task.Attempt{ended}
		tasks["child"].State, tasks["child"].Result, tasks["child"].Delivery = task.StateCancelled, nil, nil
	})
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, true, true)
	owed := recordCompletionClose(t, store, "child", "cancelled")
	beforeTasks := m.src.Tasks.List("")
	beforeVersion, err := book.ReplicaVersion()
	if err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, false, true)
	for _, item := range m.Snapshot(t.Context()).Tasks {
		if item.ID == "root" && (item.Execution != ExecutionIdle || item.Attention != 0 || item.PendingResults != 0) {
			t.Fatalf("close debt was confused with execution or delivery: %+v", item)
		}
	}
	afterVersion, err := book.ReplicaVersion()
	if err != nil || afterVersion != beforeVersion || !reflect.DeepEqual(beforeTasks, m.src.Tasks.List("")) {
		t.Fatalf("projection changed ledger/tasks: version %d/%d, err=%v", beforeVersion, afterVersion, err)
	}
	if err := store.SettleOwedClose(owed); err != nil {
		t.Fatal(err)
	}
	assertCompletionCloseViews(t, m, true, true)
	if !reflect.DeepEqual(beforeTasks, m.src.Tasks.List("")) {
		t.Fatal("refunding a native close changed task accounting or delivery")
	}
}

func assertUnreadCloseViews(t *testing.T, m *Model) {
	t.Helper()
	snap := m.Snapshot(t.Context())
	for _, item := range snap.Tasks {
		if item.CanComplete {
			t.Errorf("snapshot task %s became ready with an unread close source", item.ID)
		}
	}
	located := false
	for _, health := range snap.Sources {
		if health.Name == "ledger-task-closes" {
			located = health.Error != ""
		}
	}
	if !located {
		t.Error("unread close source was not located in source health")
	}
	if _, err := m.TaskHistory(t.Context(), task.Query{Scope: task.Scope{Kind: "children"}}); err == nil {
		t.Error("query hid the unread close source")
	}
	if _, err := m.TaskDetail(t.Context(), "root"); err == nil {
		t.Error("detail hid the unread close source")
	}
}

func TestUnreadCloseDocumentFailsClosedInEveryCompletionView(t *testing.T) {
	valid := `{"harness_id":"harness","upstream_id":"session","task_id":"removed","attempt_id":"attempt"}`
	cases := map[string]string{
		"malformed":       `{"owed_closes":[`,
		"unknown field":   `{"future_closes":[]}`,
		"nested unknown":  `{"conversations":{"chat":{"sessions":{"agent":{"future_close":true}}}}}`,
		"nested bad type": `{"conversations":[]}`,
		"two documents":   `{}` + `{}`,
		"null document":   `null`,
		"bad owed type":   `{"owed_closes":{}}`,
		"bad node type":   `{"owed_closes":[{"node_id":17,"harness_id":"harness","upstream_id":"session","task_id":"removed","attempt_id":"attempt"}]}`,
	}
	for _, field := range []string{"harness_id", "upstream_id", "task_id", "attempt_id"} {
		value := map[string]string{"harness_id": "harness", "upstream_id": "session", "task_id": "removed", "attempt_id": "attempt"}[field]
		cases["missing "+field] = `{"owed_closes":[` + strings.Replace(valid, fmt.Sprintf(`"%s":"%s"`, field, value), fmt.Sprintf(`"%s":""`, field), 1) + `]}`
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			m, book, _ := completionLandingModel(t)
			if err := book.Document("state").Save([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			assertUnreadCloseViews(t, m)
		})
	}
}

func TestMissingCloseSourceFailsClosedInEveryCompletionView(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed-ledger=%v", closed), func(t *testing.T) {
			m, book, _ := completionLandingModel(t)
			if closed {
				if err := book.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				adapter := m.src.Ledger.(Ledger)
				adapter.Book = nil
				m.src.Ledger = adapter
			}
			assertUnreadCloseViews(t, m)
		})
	}
}

func TestCompletionViewsHubLocalOwedClose(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted-node=%v", omitted), func(t *testing.T) {
			m, book, _ := completionLandingModel(t)
			store, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			owed := recordCompletionCloseOn(t, store, "grandchild", "hub-local", "")
			if omitted {
				raw, exists, err := book.Document("state").Load()
				if err != nil || !exists {
					t.Fatalf("read saved state: exists=%v err=%v", exists, err)
				}
				withoutNode := strings.Replace(string(raw), `"node_id": "",`, "", 1)
				if withoutNode == string(raw) {
					t.Fatal("fixture did not omit the owed node alias")
				}
				if err := book.Document("state").Save([]byte(withoutNode)); err != nil {
					t.Fatal(err)
				}
			}
			assertCompletionCloseViews(t, m, false, true)
			for _, ids := range [][]string{{"root", "child", "grandchild"}, {"other"}} {
				err := book.Read(t.Context(), func(tx *ledger.ReadTx) error { return state.CheckTaskDeletionTx(tx, ids) })
				if ids[0] == "root" {
					if !errors.Is(err, state.ErrCloseOwed) {
						t.Errorf("hub-local tree guard: %v, want ErrCloseOwed", err)
					}
				} else if err != nil {
					t.Errorf("hub-local debt blocked unrelated tree: %v", err)
				}
			}
			if err := store.SettleOwedClose(owed); err != nil {
				t.Fatal(err)
			}
			assertCompletionCloseViews(t, m, true, true)
		})
	}
}

func TestCompletionViewsMixedLocalAndRemoteCloses(t *testing.T) {
	for _, localFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("local-first=%v", localFirst), func(t *testing.T) {
			m, book, _ := completionLandingModel(t)
			store, err := state.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			local := recordCompletionCloseOn(t, store, "grandchild", "local", "")
			remote := recordCompletionClose(t, store, "grandchild", "remote")
			other := recordCompletionClose(t, store, "other", "other")
			assertCompletionCloseViews(t, m, false, false)
			stale := local
			stale.NodeID = "node"
			if err := store.SettleOwedClose(stale); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(store.OwedCloses(), []state.OwedClose{local, remote, other}) {
				t.Fatal("a different node identity refunded the hub-local obligation")
			}
			assertCompletionCloseViews(t, m, false, false)
			first, last := remote, local
			if localFirst {
				first, last = local, remote
			}
			if err := store.SettleOwedClose(first); err != nil {
				t.Fatal(err)
			}
			assertCompletionCloseViews(t, m, false, false)
			if err := store.SettleOwedClose(last); err != nil {
				t.Fatal(err)
			}
			assertCompletionCloseViews(t, m, true, false)
			if err := store.SettleOwedClose(other); err != nil {
				t.Fatal(err)
			}
			assertCompletionCloseViews(t, m, true, true)
		})
	}
}
