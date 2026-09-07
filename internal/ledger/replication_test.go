package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testReplicator struct {
	book   *Ledger
	before func(ReplicatedWrite) error
	mu     sync.Mutex
	writes []ReplicatedWrite
}

func (r *testReplicator) Prepare(context.Context) (ReplicaPosition, error) {
	v, err := r.book.ReplicaVersion()
	return ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}

func (r *testReplicator) Propose(_ context.Context, w ReplicatedWrite) ([]byte, error) {
	r.mu.Lock()
	r.writes = append(r.writes, w)
	r.mu.Unlock()
	if r.before != nil {
		if err := r.before(w); err != nil {
			return nil, err
		}
	}
	return r.book.ApplyReplicated(w.ID, w.ExpectedVersion+1, w.Payload)
}

func replicaBook(t *testing.T) (*Ledger, *testReplicator) {
	t.Helper()
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	r := &testReplicator{book: l}
	if err := l.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	return l, r
}

func TestReplicatedDocumentInvisibleUntilCommitAndRejectedWriteRollsBack(t *testing.T) {
	l, r := replicaBook(t)
	proposed, release := make(chan struct{}), make(chan struct{})
	r.before = func(ReplicatedWrite) error { close(proposed); <-release; return nil }
	done := make(chan error, 1)
	go func() { done <- l.Document("session").Save([]byte(`{"task":42}`)) }()
	<-proposed
	if _, ok, err := l.Document("session").Load(); err != nil || ok {
		t.Fatalf("uncommitted document visible: ok=%v err=%v", ok, err)
	}
	select {
	case err := <-done:
		t.Fatalf("write returned before commit: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, ok, err := l.Document("session").Load(); err != nil || !ok || string(got) != `{"task":42}` {
		t.Fatalf("committed document=%s ok=%v err=%v", got, ok, err)
	}
	rejected := errors.New("quorum unavailable")
	r.before = func(ReplicatedWrite) error { return rejected }
	if err := l.Document("session").Save([]byte(`{"task":43}`)); !errors.Is(err, rejected) {
		t.Fatalf("rejection=%v", err)
	}
	got, _, _ := l.Document("session").Load()
	if string(got) != `{"task":42}` {
		t.Fatalf("rejected document survived: %s", got)
	}
}

func TestReplicatedTransitionPublishesLeaseEventAndBindingTogether(t *testing.T) {
	l, r := replicaBook(t)
	ctx := t.Context()
	lease, err := l.Acquire(ctx, "attempt:1", "worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Begin(ctx, "attempt-1", "attempt", "running", "worker", nil); err != nil {
		t.Fatal(err)
	}
	proposed, release := make(chan struct{}), make(chan struct{})
	r.before = func(ReplicatedWrite) error { close(proposed); <-release; return nil }
	done := make(chan error, 1)
	go func() {
		_, err := l.Transition(ctx, "attempt-1", "running", "succeeded", "worker", []Lease{lease}, nil, func(tx *Tx, _ *Operation) error {
			if _, err := tx.CompareAndSetName("result/1", 0, "artifact"); err != nil {
				return err
			}
			return tx.PutBinding("completion", "1", map[string]string{"answer": "done"})
		})
		done <- err
	}()
	<-proposed
	op, _, _ := l.Operation(ctx, "attempt-1")
	_, named, _ := l.Name(ctx, "result/1")
	events, _ := l.Events(ctx, "attempt-1")
	if op.State != "running" || named || len(events) != 1 {
		t.Fatalf("partial transition visible: op=%+v named=%v events=%d", op, named, len(events))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	op, _, _ = l.Operation(ctx, "attempt-1")
	_, named, _ = l.Name(ctx, "result/1")
	events, _ = l.Events(ctx, "attempt-1")
	if op.State != "succeeded" || !named || len(events) != 2 {
		t.Fatalf("transition incomplete: op=%+v named=%v events=%d", op, named, len(events))
	}
}

func TestReplicatedLeaseChangesCannotBeAcknowledgedWithoutCommit(t *testing.T) {
	l, r := replicaBook(t)
	ctx := t.Context()
	first, err := l.Acquire(ctx, "worker", "attempt-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := l.Acquire(ctx, "slot", "attempt-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("proposal rejected")
	r.before = func(ReplicatedWrite) error { return rejected }
	checks := []struct {
		name string
		run  func() error
	}{
		{"acquire", func() error { _, err := l.Acquire(ctx, "new", "other", time.Hour); return err }},
		{"renew", func() error { _, err := l.Renew(ctx, first, 2*time.Hour); return err }},
		{"transfer", func() error { _, err := l.Transfer(ctx, first, "other", time.Hour); return err }},
		{"release", func() error { return l.Release(ctx, first) }},
		{"invalidate", func() error { return l.Invalidate(ctx, first.Key) }},
		{"invalidate holder", func() error { _, err := l.InvalidateHeldBy(ctx, "attempt-1"); return err }},
		{"invalidate all", func() error { return l.InvalidateAll(ctx) }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, rejected) {
				t.Fatalf("rejection=%v", err)
			}
			for _, original := range []Lease{first, other} {
				got, _, err := l.LeaseOf(ctx, original.Key)
				if err != nil || got.Key != original.Key || got.Holder != original.Holder || got.Incarnation != original.Incarnation || got.Epoch != original.Epoch || !got.ExpiresAt.Equal(original.ExpiresAt) {
					t.Fatalf("lease changed: %+v original=%+v err=%v", got, original, err)
				}
			}
		})
	}
	if _, ok, _ := l.LeaseOf(ctx, "new"); ok {
		t.Fatal("rejected acquisition survived")
	}
	r.before = nil
	before := len(r.writes)
	keys, err := l.InvalidateHeldBy(ctx, "attempt-1")
	if err != nil || len(keys) != 2 || len(r.writes) != before+1 {
		t.Fatalf("holder invalidation must be one batch: keys=%v err=%v batches=%d", keys, err, len(r.writes)-before)
	}
}

func TestReplicatedValidationRejectsBeforeProposalAndLeavesNoPartialMutation(t *testing.T) {
	l, r := replicaBook(t)
	ctx := t.Context()
	lease, err := l.Acquire(ctx, "held", "a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Begin(ctx, "a", "attempt", "running", "a", nil); err != nil {
		t.Fatal(err)
	}
	before := len(r.writes)
	if _, err := l.Acquire(ctx, "held", "b", time.Hour); !errors.Is(err, ErrHeld) {
		t.Fatalf("held lease = %v", err)
	}
	lease.Epoch++
	if _, err := l.Transition(ctx, "a", "running", "succeeded", "a", []Lease{lease}, nil, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale transition=%v", err)
	}
	rejected := errors.New("completion validation failed")
	if _, err := l.Transition(ctx, "a", "running", "succeeded", "a", nil, nil, func(tx *Tx, _ *Operation) error {
		if err := tx.PutBinding("completion", "a", "partial"); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if len(r.writes) != before {
		t.Fatal("business rejection reached consensus")
	}
	var value string
	if ok, err := l.GetBinding(ctx, "completion", "a", &value); err != nil || ok {
		t.Fatalf("partial mutation persisted: %v %v", ok, err)
	}
	if op, _, _ := l.Operation(ctx, "a"); op.State != "running" {
		t.Fatalf("rejected transition changed state: %+v", op)
	}
}

func TestReplicatedSnapshotRestorePersistsEveryFactAndReplayReceipt(t *testing.T) {
	l, r := replicaBook(t)
	ctx := t.Context()
	lease, err := l.Acquire(ctx, "resource", "attempt-9", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Begin(ctx, "attempt-9", "attempt", "running", "worker", map[string]int{"attempt": 9}); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(ctx, func(tx *Tx) error {
		if _, err := tx.CompareAndSetName("result/task-9", 0, "artifact-9"); err != nil {
			return err
		}
		return tx.PutBinding("sessions", "original", map[string]int{"task": 9})
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Document("tasks").Save([]byte(`{"9":{"state":"running"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Command(ctx, "cmd-9", "message", "user", func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"accepted":true}`), nil }); err != nil {
		t.Fatal(err)
	}
	effect := EffectID{Operation: "attempt-9", Kind: "message", InstanceKey: "1"}
	if _, err := l.Journal().Started(effect, "cmd-9", map[string]string{"target": "room"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Journal().Confirmed(effect, map[string]string{"receipt": "r9"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	restored, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.PutBinding(ctx, "unsafe", "write", true); !errors.Is(err, ErrReplicaUnavailable) {
		t.Fatalf("replicated reopen accepted unfenced write: %v", err)
	}
	r2 := &testReplicator{book: restored}
	if err := restored.AttachReplication(r2); err != nil {
		t.Fatal(err)
	}
	for _, write := range r.writes {
		if _, err := restored.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload); err != nil {
			t.Fatalf("durable replay receipt lost: %v", err)
		}
	}
	if version, err := restored.ReplicaVersion(); err != nil || version != uint64(len(r.writes)) {
		t.Fatalf("replica version=%d err=%v", version, err)
	}
	if got, _, _ := restored.LeaseOf(ctx, "resource"); got.Holder != lease.Holder || got.Incarnation != lease.Incarnation || got.Epoch != lease.Epoch || !got.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("lease lost: %+v", got)
	}
	if op, ok, err := restored.Operation(ctx, "attempt-9"); err != nil || !ok || op.State != "running" {
		t.Fatalf("operation lost: %+v %v %v", op, ok, err)
	}
	if events, _ := restored.Events(ctx, "attempt-9"); len(events) != 1 {
		t.Fatalf("event replayed twice: %d", len(events))
	}
	if name, ok, _ := restored.Name(ctx, "result/task-9"); !ok || name.Artifact != "artifact-9" {
		t.Fatalf("name lost: %+v", name)
	}
	if got, ok, _ := restored.Document("tasks").Load(); !ok || string(got) != `{"9":{"state":"running"}}` {
		t.Fatalf("document lost: %s", got)
	}
	if result, replayed, err := restored.Command(ctx, "cmd-9", "message", "user", func(context.Context) (json.RawMessage, error) {
		t.Fatal("replayed business command ran")
		return nil, nil
	}); err != nil || !replayed || string(result) != `{"accepted":true}` {
		t.Fatalf("command receipt lost: %s %v %v", result, replayed, err)
	}
	outcomes, err := restored.Journal().Reconcile()
	if err != nil || len(outcomes) != 1 || !outcomes[0].Known() {
		t.Fatalf("effect evidence lost: %+v %v", outcomes, err)
	}
	if _, err := restored.Transition(ctx, "attempt-9", "running", "succeeded", "new-worker", []Lease{lease}, nil, nil); err != nil {
		t.Fatal(err)
	}
	events, _ := restored.Events(ctx, "attempt-9")
	if len(events) != 2 || events[1].Seq != events[0].Seq+1 {
		t.Fatalf("SQLite sequence lost: %+v", events)
	}
}

func TestReplicatedSQLArgumentsRetainIntegerBlobAndNullTypes(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.DB().Exec(`CREATE TABLE typed_facts (id INTEGER PRIMARY KEY, content BLOB, empty BLOB, optional TEXT, ratio REAL)`); err != nil {
		t.Fatal(err)
	}
	r := &testReplicator{book: l}
	if err := l.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	id := int64(math.MaxInt64 - 9)
	blob := []byte{0, 255, 128, 1}
	if err := l.Update(t.Context(), func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO typed_facts(id, content, empty, optional, ratio) VALUES (?, ?, ?, ?, ?)`, id, blob, []byte{}, nil, 1.25)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := l.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	var gotID int64
	var gotBlob []byte
	var idType, blobType, emptyType, nullType string
	var ratio float64
	if err := replica.DB().QueryRow(`SELECT id, content, ratio, typeof(id), typeof(content), typeof(empty), typeof(optional) FROM typed_facts`).Scan(&gotID, &gotBlob, &ratio, &idType, &blobType, &emptyType, &nullType); err != nil {
		t.Fatal(err)
	}
	if gotID != id || !bytes.Equal(gotBlob, blob) || ratio != 1.25 || idType != "integer" || blobType != "blob" || emptyType != "blob" || nullType != "null" {
		t.Fatalf("type corruption: %d %v %g %s %s %s %s", gotID, gotBlob, ratio, idType, blobType, emptyType, nullType)
	}
}

func TestReplicatedApplyFailureRollsBackWholeBatchAndStopsWrites(t *testing.T) {
	l, r := replicaBook(t)
	r.before = func(ReplicatedWrite) error { return errors.New("capture batch") }
	_ = l.PutBinding(t.Context(), "facts", "1", true)
	write := r.writes[0]
	var batch mutationBatch
	if err := json.Unmarshal(write.Payload, &batch); err != nil {
		t.Fatal(err)
	}
	batch.Statements = append(batch.Statements, sqlMutation{SQL: `INSERT INTO missing_table(id) VALUES (1)`, Rows: 1})
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplyReplicated(write.ID, 1, payload); !errors.Is(err, ErrReplicaFailed) {
		t.Fatalf("bad committed batch error=%v", err)
	}
	var value bool
	if ok, err := l.GetBinding(t.Context(), "facts", "1", &value); err != nil || ok {
		t.Fatalf("partial apply survived: %v %v", ok, err)
	}
	if version, _ := l.ReplicaVersion(); version != 0 {
		t.Fatalf("failed apply advanced version: %d", version)
	}
	if err := l.PutBinding(t.Context(), "facts", "2", true); !errors.Is(err, ErrReplicaFailed) {
		t.Fatalf("failed replica continued writing: %v", err)
	}
}

func TestCachedDatabaseAndQueryMethodsCannotBypassReplication(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	db := l.DB()
	r := &testReplicator{book: l}
	if err := l.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES ('x','1','null','now')`); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("cached database bypass=%v", err)
	}
	if err := l.Update(t.Context(), func(tx *Tx) error { return tx.QueryRow(`DELETE FROM bindings RETURNING id`).Scan(new(string)) }); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("query bypass=%v", err)
	}
	if err := l.Update(t.Context(), func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES ('x','1','null','now'); COMMIT`)
		return err
	}); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("transaction bypass=%v", err)
	}
	if err := l.Update(t.Context(), func(tx *Tx) error { _, err := tx.Exec(`UPDATE replica_state SET version = 999`); return err }); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("version bypass=%v", err)
	}
	for _, statement := range []string{`DELETE FROM 'replica_state'`, `DELETE FROM 'replica_commands'`, `DELETE FROM main.'replica_commands'`} {
		if err := l.Update(t.Context(), func(tx *Tx) error { _, err := tx.Exec(statement); return err }); !errors.Is(err, ErrReplicaWriteBypass) {
			t.Fatalf("quoted metadata bypass %q: %v", statement, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO bindings(kind,id,data,updated_at) SELECT 'diag','1',file,'x' FROM pragma_database_list WHERE name='main'`,
		`INSERT INTO bindings(kind,id,data,updated_at) VALUES ('diag','1',sqlite_version(),'x')`,
		`INSERT INTO bindings(kind,id,data,updated_at) VALUES ('diag','1',quote(1),'x')`,
		"INSERT INTO bindings(kind,id,data,updated_at) VALUES ('diag','1',timediff\f('now','2000-01-01'),'x')",
		`UPDATE meta SET value = '999' WHERE key = 'incarnation'`,
		`UPDATE OR FAIL bindings SET data = 'partial'`,
		`UPDATE OR ROLLBACK bindings SET data = 'partial'`,
	} {
		if err := l.Update(t.Context(), func(tx *Tx) error { _, err := tx.Exec(statement); return err }); !errors.Is(err, ErrReplicaWriteBypass) {
			t.Fatalf("non-deterministic or metadata SQL %q: %v", statement, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.DB().Exec(`DELETE FROM bindings`); !errors.Is(err, ErrReplicaUnavailable) {
		t.Fatalf("reopen bypass=%v", err)
	}
}

func TestReplicatedEffectStartWaitsForQuorumBeforeExternalAction(t *testing.T) {
	l, r := replicaBook(t)
	proposed, release := make(chan struct{}), make(chan struct{})
	r.before = func(ReplicatedWrite) error { close(proposed); <-release; return nil }
	done := make(chan error, 1)
	go func() {
		_, err := l.Journal().Started(EffectID{Operation: "attempt-1", Kind: "send", InstanceKey: "1"}, "cmd", nil)
		done <- err
	}()
	<-proposed
	outcomes, err := l.Journal().Reconcile()
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("uncommitted effect visible: %+v %v", outcomes, err)
	}
	select {
	case err := <-done:
		t.Fatalf("effect released before quorum: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	outcomes, err = l.Journal().Reconcile()
	if err != nil || len(outcomes) != 1 || outcomes[0].Known() {
		t.Fatalf("effect outcome must remain unknown: %+v %v", outcomes, err)
	}
}

func TestReplicatedCommandRunsOnlyAfterReservationCommitAndWaitsForResultCommit(t *testing.T) {
	l, r := replicaBook(t)
	proposed, release := make(chan ReplicatedWrite), make(chan struct{})
	r.before = func(w ReplicatedWrite) error { proposed <- w; <-release; return nil }
	ran := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := l.Command(t.Context(), "input-1", "task", "user", func(context.Context) (json.RawMessage, error) {
			close(ran)
			return json.RawMessage(`{"done":true}`), nil
		})
		done <- err
	}()
	<-proposed
	select {
	case <-ran:
		t.Fatal("external callback ran before its command reservation committed")
	default:
	}
	release <- struct{}{}
	<-ran
	<-proposed
	select {
	case err := <-done:
		t.Fatalf("command returned before its result committed: %v", err)
	default:
	}
	var finished any
	if err := l.DB().QueryRow(`SELECT finished_at FROM commands WHERE id = ?`, "input-1").Scan(&finished); err != nil || finished != nil {
		t.Fatalf("uncommitted result visible: %v %v", finished, err)
	}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplicatedCrashReplayDoesNotApplyEventOrEffectTwice(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &testReplicator{book: l}
	if err := l.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Begin(t.Context(), "attempt-1", "attempt", "running", "worker", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Journal().Started(EffectID{Operation: "attempt-1", Kind: "send", InstanceKey: "1"}, "input-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, write := range r.writes {
		if _, err := l.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if version, _ := l.ReplicaVersion(); version != 2 {
		t.Fatalf("replay advanced version: %d", version)
	}
	if events, _ := l.Events(t.Context(), "attempt-1"); len(events) != 1 {
		t.Fatalf("event replayed: %+v", events)
	}
	if entries, _ := l.effectEntries(0); len(entries) != 1 {
		t.Fatalf("effect replayed: %+v", entries)
	}
	first := r.writes[0]
	if _, err := l.ApplyReplicated(first.ID, 1, []byte(`{"different":true}`)); !errors.Is(err, ErrReplicaFailed) {
		t.Fatalf("changed committed payload not refused: %v", err)
	}
}

func TestReplicaCannotClaimAcknowledgementWithoutLocalApplication(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.AttachReplication(missingApply{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Document("tasks").Save([]byte(`{}`)); !errors.Is(err, ErrReplicaFailed) {
		t.Fatalf("early acknowledgement accepted: %v", err)
	}
	if _, ok, _ := l.Document("tasks").Load(); ok {
		t.Fatal("unapplied document visible")
	}
}

type missingApply struct{}

func (missingApply) Prepare(context.Context) (ReplicaPosition, error) {
	return ReplicaPosition{CoordinatorEpoch: 1}, nil
}
func (missingApply) Propose(context.Context, ReplicatedWrite) ([]byte, error) { return nil, nil }

func TestReplicaRestoreIntentRepairsIncarnationAcrossEitherCrashBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before SQLite restore", true: "after SQLite restore"}[committed], func(t *testing.T) {
			dir := t.TempDir()
			book, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := book.AttachReplication(&testReplicator{book: book}); err != nil {
				t.Fatal(err)
			}
			if err := (&FileDocument{Path: filepath.Join(dir, replicaRestoreFile)}).Save([]byte(`{"from":1,"to":2}`)); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := setMetaUint(book.db, "incarnation", 2); err != nil {
					t.Fatal(err)
				}
			}
			if err := book.Close(); err != nil {
				t.Fatal(err)
			}
			book, err = Open(dir, Options{})
			if err != nil {
				t.Fatalf("restore crash prevented Raft recovery: %v", err)
			}
			defer book.Close()
			want := uint64(1)
			if committed {
				want = 2
			}
			if book.Incarnation() != want {
				t.Fatalf("incarnation=%d want=%d", book.Incarnation(), want)
			}
			if _, err := os.Stat(filepath.Join(dir, replicaRestoreFile)); !os.IsNotExist(err) {
				t.Fatalf("restore intent not cleared: %v", err)
			}
			if err := book.Document("tasks").Save([]byte(`{}`)); !errors.Is(err, ErrReplicaUnavailable) {
				t.Fatalf("repair enabled unfenced writes: %v", err)
			}
		})
	}
}

func TestReplicaWriterUsesCoreApplyAndRetainsJournalEvidence(t *testing.T) {
	dir := t.TempDir()
	core, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	r := &testReplicator{book: core}
	if err := core.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(dir, Options{ReplicaWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	if err := writer.Document("tasks").Save([]byte(`{"task":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Journal().Started(EffectID{Operation: "attempt-1", Kind: "send", InstanceKey: "1"}, "cmd", nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := core.journal.readFileAll(); err != nil || len(entries) != 1 {
		t.Fatalf("business handle replaced core journal: %+v %v", entries, err)
	}
	if _, err := writer.SnapshotReplica(); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("business handle captured application snapshot: %v", err)
	}
	if _, err := writer.ApplyReplicated("x", 2, []byte(`{}`)); !errors.Is(err, ErrReplicaWriteBypass) {
		t.Fatalf("business handle applied command: %v", err)
	}
	if err := setMetaUint(core.db, "incarnation", 2); err != nil {
		t.Fatal(err)
	}
	if err := writer.Document("tasks").Save([]byte(`{"task":2}`)); !errors.Is(err, ErrStale) {
		t.Fatalf("old writer survived incarnation restore: %v", err)
	}
}

func TestReplicaRefusesSchemaWithHiddenOrAmbientWrites(t *testing.T) {
	for _, schema := range []string{
		`CREATE TRIGGER hidden BEFORE INSERT ON bindings BEGIN DELETE FROM replica_commands; END`,
		`CREATE TABLE ambient (id INTEGER PRIMARY KEY, created TEXT DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TEMP TABLE bindings(kind TEXT,id TEXT,data TEXT,updated_at TEXT)`,
	} {
		l, err := Open(t.TempDir(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.DB().Exec(schema); err != nil {
			t.Fatal(err)
		}
		if err := l.AttachReplication(&testReplicator{book: l}); !errors.Is(err, ErrReplicaWriteBypass) {
			t.Fatalf("unsupported schema allowed %q: %v", schema, err)
		}
		_ = l.Close()
	}
}
