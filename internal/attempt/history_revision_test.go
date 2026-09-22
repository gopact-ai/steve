package attempt

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func historyToken(t *testing.T, book *ledger.Ledger, taskID string) string {
	t.Helper()
	var token string
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		var err error
		token, err = historyRevisionTx(tx, taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func historyOpen(t *testing.T, service *Service, id, taskID string) Record {
	t.Helper()
	record, err := service.Open(t.Context(), Spec{ID: id, TaskID: taskID, Scope: ScopeNone, Kind: KindChat})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestNativeHistoryRevisionTracksSameStateWritesAndScopes(t *testing.T) {
	s, _ := newService(t)
	for _, id := range []string{"a", "b", "c"} {
		historyOpen(t, s, id, "selected")
	}
	other := historyOpen(t, s, "other", "unrelated")
	var previous string
	for _, write := range []func() error{
		func() error { _, err := s.Advance(t.Context(), "a", Prepared, "test", nil); return err },
		func() error { _, err := s.Advance(t.Context(), "a", Running, "test", nil); return err },
		func() error { return s.MarkSessionSettled(t.Context(), "a", "test") },
		func() error { return s.ArmSession(t.Context(), "a", "test") },
		func() error { return s.ReleaseEndpointAfterSessionClosed(t.Context(), "a", "test") },
		func() error { return s.MarkUnsettled(t.Context(), "a", "test", errors.New("unknown"), nil) },
		func() error { _, err := s.ConfirmStopped(t.Context(), "a", "test", "confirmed"); return err },
	} {
		first, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Limit: 1})
		if err != nil || first.NextCursor == "" {
			t.Fatalf("first page: %+v %v", first, err)
		}
		previous = historyToken(t, s.l, "selected")
		unrelated := historyToken(t, s.l, other.TaskID)
		if err := write(); err != nil {
			t.Fatal(err)
		}
		if historyToken(t, s.l, "selected") == previous || historyToken(t, s.l, other.TaskID) != unrelated {
			t.Fatal("owner mutation missed or escaped its scope")
		}
		if _, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Cursor: first.NextCursor}); !errors.Is(err, ErrHistoryChanged) {
			t.Fatalf("selected mutation did not invalidate page: %v", err)
		}
	}
	previous = historyToken(t, s.l, "selected")
	if _, err := s.Advance(t.Context(), "a", Running, "invalid", nil); err == nil {
		t.Fatal("fixture: invalid transition succeeded")
	}
	if historyToken(t, s.l, "selected") != previous {
		t.Fatal("rejected transition changed revision")
	}
}

func TestNativeHistoryScopeClosureInvalidatesOnlyItsConversation(t *testing.T) {
	s, _ := newService(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := tasks.Create(task.Task{Transport: "console", Channel: "selected"})
	if err != nil {
		t.Fatal(err)
	}
	historyOpen(t, s, "a", root.ID)
	historyOpen(t, s, "b", root.ID)
	first, err := s.QueryHistory(t.Context(), HistoryQuery{Conversation: "selected", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{Transport: "console", Channel: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	q := HistoryQuery{Conversation: "selected", Limit: 1, Cursor: first.NextCursor}
	if _, err := s.QueryHistory(t.Context(), q); err != nil {
		t.Fatal("unrelated task creation invalidates scope", err)
	}
	if _, err := tasks.Create(task.Task{Transport: "console", Parent: root.ID, Channel: "delegated-channel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueryHistory(t.Context(), q); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("descendant closure changed without invalidation", err)
	}
}

type historyReplicator struct {
	book    *ledger.Ledger
	reject  error
	payload []byte
}

func (r *historyReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}
func (r *historyReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.payload = append([]byte(nil), write.Payload...)
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func TestNativeHistoryRevisionReplicationRollbackRestoreAndReopen(t *testing.T) {
	s, now := newService(t)
	replica := &historyReplicator{book: s.l}
	if err := s.l.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		historyOpen(t, s, id, "selected")
	}
	first, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	oldToken := historyToken(t, s.l, "selected")
	replica.reject = errors.New("no quorum")
	if _, err := s.Advance(t.Context(), "a", Prepared, "test", nil); !errors.Is(err, replica.reject) {
		t.Fatal("write was not rejected", err)
	}
	if historyToken(t, s.l, "selected") != oldToken {
		t.Fatal("rejected proposal published read revision")
	}
	next, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Cursor: first.NextCursor, Limit: 1})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != "b" {
		t.Fatal("rollback invalidated traversal", next, err)
	}
	replica.reject = nil
	snapshot, err := s.l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(t.Context(), "a", Prepared, "branch-a", nil); err != nil {
		t.Fatal(err)
	}
	branchToken := historyToken(t, s.l, "selected")
	dir := t.TempDir()
	target, err := ledger.Open(dir, ledger.Options{Now: now.now})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if got := historyToken(t, target, "selected"); got != oldToken {
		t.Fatal("restore did not preserve exact revision")
	}
	restored := New(target)
	got, err := restored.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected", Cursor: first.NextCursor, Limit: 1})
	if err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("restored snapshot cannot continue its cursor", got, err)
	}
	if err := target.AttachReplication(&historyReplicator{book: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Advance(t.Context(), "a", Prepared, "branch-b", nil); err != nil {
		t.Fatal(err)
	}
	newToken := historyToken(t, target, "selected")
	if newToken == branchToken || newToken == oldToken {
		t.Fatal("restored branch reused a prior mutation generation")
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	target, err = ledger.Open(dir, ledger.Options{Now: now.now})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if historyToken(t, target, "selected") != newToken {
		t.Fatal("reopen lost read generation")
	}
}

func TestNativeHistoryRevisionMissingOrCorruptFailsClosedWithoutLazyWrites(t *testing.T) {
	for _, raw := range []string{"", `"short"`, `null`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			s, _ := newService(t)
			historyOpen(t, s, "a", "selected")
			if raw == "" {
				if _, err := s.l.DB().Exec(`DELETE FROM bindings WHERE kind=?`, historyRevisionKind); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.l.DB().Exec(`UPDATE bindings SET data=? WHERE kind=?`, raw, historyRevisionKind); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := s.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected"}); err == nil {
					t.Fatal("missing/corrupt read revision became a healthy page")
				}
			}
			if _, err := s.Advance(t.Context(), "a", Prepared, "test", nil); err == nil {
				t.Fatal("normal mutation repaired missing revision without import")
			}
			if raw == "" {
				var n int
				if err := s.l.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind=?`, historyRevisionKind).Scan(&n); err != nil || n != 0 {
					t.Fatal("read or rejected write lazily initialized revision", n, err)
				}
			}
			snapshot, err := s.l.SnapshotReplica()
			if err != nil {
				t.Fatal(err)
			}
			target, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			if err := target.RestoreReplica(snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := New(target).QueryHistory(t.Context(), HistoryQuery{TaskID: "selected"}); err == nil {
				t.Fatal("restore silently repaired missing/corrupt revision")
			}
		})
	}
}

func TestNativeHistoryImportRevisionSharesAtomicOwnerBoundary(t *testing.T) {
	source, _ := newService(t)
	historyOpen(t, source, "a", "selected")
	facts, err := source.l.ExportOperations(t.Context(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := newService(t)
	owner := func(tx *ledger.Tx, validateOnly bool) error {
		return ImportHistoryTx(tx, facts.Operations, validateOnly)
	}
	rejected := errors.New("other owner rejected")
	err = target.l.ImportFacts(t.Context(), facts, nil, nil, owner, func(_ *ledger.Tx, validateOnly bool) error {
		if validateOnly {
			return nil
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if token := historyToken(t, target.l, "selected"); token != "" {
		t.Fatal("import revision escaped rollback")
	}
	if _, exists, err := target.l.Operation(t.Context(), "a"); err != nil || exists {
		t.Fatal("import operation escaped rollback", err)
	}
	if err := target.l.ImportFacts(t.Context(), facts, nil, nil, owner); err != nil {
		t.Fatal(err)
	}
	if historyToken(t, target.l, "selected") == historyToken(t, source.l, "selected") {
		t.Fatal("import reused source read-generation authority")
	}
	page, err := target.QueryHistory(t.Context(), HistoryQuery{TaskID: "selected"})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "a" {
		t.Fatal("imported history not readable", page, err)
	}
}

func TestNativeHistoryRevisionRelocationAndSessionWrites(t *testing.T) {
	s, _, old, plan, _, _ := relocationFixture(t)
	before := historyToken(t, s.l, old.TaskID)
	if _, err := s.OpenRelocation(t.Context(), plan.ID, RelocationApproval{}); err == nil {
		t.Fatal("invalid relocation accepted")
	}
	if historyToken(t, s.l, old.TaskID) != before {
		t.Fatal("rejected relocation changed revision")
	}
	replacement, err := s.OpenRelocation(t.Context(), plan.ID, manualRelocation(plan))
	if err != nil {
		t.Fatal(err)
	}
	after := historyToken(t, s.l, old.TaskID)
	if after == before {
		t.Fatal("direct relocation SQL missed history revision")
	}
	if err := s.RecordRelocationSession(t.Context(), replacement.ID, RelocationSessionConfig{Fingerprint: "test"}); err != nil {
		t.Fatal(err)
	}
	if historyToken(t, s.l, old.TaskID) == after {
		t.Fatal("same-state session-envelope write missed history revision")
	}
}

func TestNativeHistoryRevisionRetainedAndStopOwnerWrites(t *testing.T) {
	for _, action := range []string{"record-session", "retained", "task-stop", "process-stop", "expire"} {
		t.Run(action, func(t *testing.T) {
			s, _, old, proof, tasks := retainedFixture(t)
			before := historyToken(t, s.l, old.TaskID)
			var err error
			switch action {
			case "record-session":
				_, err = s.RecordSession(t.Context(), old.ID, "test", old.Session)
			case "retained":
				_, err = s.RecoverRetained(t.Context(), old.ID, proof)
			case "task-stop":
				if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				proof.Session.Command.State, proof.Session.Command.Settled = "cancelled", true
				proof.Session.State = "idle"
				_, err = s.ConfirmTaskStopped(t.Context(), old.ID, "test", proof)
			case "process-stop":
				proof.Session.Command = nil
				proof.Session.State, proof.Session.ProcessStopped = "closed", true
				_, err = s.ConfirmProcessStopped(t.Context(), old.ID, "test", proof)
			case "expire":
				_, err = s.ExpireAll(t.Context(), "test")
			}
			if err != nil {
				t.Fatal(err)
			}
			if historyToken(t, s.l, old.TaskID) == before {
				t.Fatal("owner mutation omitted history revision")
			}
		})
	}
}

// Every production owner SetData must pass through the one save boundary.
// This prevents new same-state mutation paths silently forgetting pagination.
func TestAttemptRecordSavesUseHistoryBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "history_revision.go" {
			continue
		}
		root, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(root, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selected, ok := call.Fun.(*ast.SelectorExpr); ok && selected.Sel.Name == "SetData" {
					t.Errorf("%s bypasses owner history save boundary", path)
				}
			}
			return true
		})
	}
}

func TestNativeHistoryReadAndWriteCostDoNotScaleWithTaskHistory(t *testing.T) {
	var baseline float64
	var bytes int
	for _, n := range []int{24, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, _ := newService(t)
			historyOpen(t, s, "live", "selected")
			if _, err := s.l.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
				INSERT INTO operations SELECT 'history-'||n,'attempt','bound',1,1,json_object('id','history-'||n,'task_id','selected','started_at','2026-09-01T00:00:00Z'),'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`, n); err != nil {
				t.Fatal(err)
			}
			q := HistoryQuery{TaskID: "selected", Limit: 1}
			allocations := testing.AllocsPerRun(10, func() {
				page, err := s.QueryHistory(t.Context(), q)
				if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
					t.Fatal(page, err)
				}
			})
			replica := &historyReplicator{book: s.l}
			if err := s.l.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Advance(t.Context(), "live", Prepared, "test", nil); err != nil {
				t.Fatal(err)
			}
			// The driver's own allocations drift by a few dozen between
			// runs (pooled connections, statement caches); a query that
			// walked the history would cost thousands more at this size.
			if n == 24 {
				baseline, bytes = allocations, len(replica.payload)
			} else if allocations > baseline*1.5 || len(replica.payload) > bytes+50 {
				t.Fatalf("history-dependent query/write: allocations %.0f -> %.0f, bytes %d -> %d", baseline, allocations, bytes, len(replica.payload))
			}
			t.Logf("history=%d query allocations=%.0f replicated mutation bytes=%d", n, allocations, len(replica.payload))
		})
	}
}
