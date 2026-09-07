package workflowstore

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/gopact/gopacttest"
	"github.com/gopact-ai/gopact/runlog"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ledger"
)

type testReplicator struct {
	book   *ledger.Ledger
	reject error
	calls  int
}

func checkpoint(runID string) workflow.CheckpointRecord {
	now := time.Now().UTC()
	return workflow.CheckpointRecord{ID: "checkpoint-" + runID, SessionID: "session", RunID: runID, WorkflowName: "plan", TopologyVersion: "v1", SchemaVersion: 2, Version: 1, Status: workflow.CheckpointRunning, ReplayStatus: workflow.ReplayUnknown, Payload: []byte(`{"step":"first"}`), OwnerID: "owner", ClaimSequence: 1, LeaseExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
}

func event(runID string, seq int64) runlog.Record {
	return runlog.Record{SessionID: "session", RunID: runID, Sequence: seq, Source: "workflow", EventType: "step.completed", Timestamp: time.Unix(1, 0).UTC(), Payload: []byte(`{"answer":"done"}`)}
}

func TestRejectedCheckpointAndLogMutationsLeaveNoValuesOrIndexes(t *testing.T) {
	store, replica := newStore(t)
	rejected := errors.New("quorum unavailable")
	replica.reject = rejected
	if err := store.Create(t.Context(), checkpoint("rejected")); !errors.Is(err, rejected) {
		t.Fatalf("create rejection=%v", err)
	}
	if _, err := store.Load(t.Context(), "rejected"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("uncommitted checkpoint visible: %v", err)
	}
	replica.reject = nil
	seed := checkpoint("run")
	if err := store.Create(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	replica.reject = rejected
	saved := seed
	saved.Payload = []byte(`{"step":"not committed"}`)
	if err := store.Save(t.Context(), saved, seed.Version); !errors.Is(err, rejected) {
		t.Fatalf("save rejection=%v", err)
	}
	if err := store.AppendFenced(t.Context(), event("run", 1), runlog.Fence{OwnerID: seed.OwnerID, ClaimSequence: seed.ClaimSequence}); !errors.Is(err, rejected) {
		t.Fatalf("log rejection=%v", err)
	}
	current, err := store.Load(t.Context(), seed.RunID)
	if err != nil || current.Version != 1 || string(current.Payload) != string(seed.Payload) {
		t.Fatalf("rejected checkpoint survived: %+v %v", current, err)
	}
	for _, query := range []runlog.Query{{}, {RunID: "run"}, {SessionID: "session"}} {
		if events, err := store.List(t.Context(), query); err != nil || len(events) != 0 {
			t.Fatalf("rejected log/index survived: %+v %v", events, err)
		}
	}
	replica.reject = nil
	if err := store.AppendFenced(t.Context(), event("run", 1), runlog.Fence{OwnerID: seed.OwnerID, ClaimSequence: seed.ClaimSequence}); err != nil {
		t.Fatal(err)
	}
	if events, err := store.List(t.Context(), runlog.Query{RunID: "run"}); err != nil || len(events) != 1 {
		t.Fatalf("rejected key blocked future append: %+v %v", events, err)
	}
}

func TestHistoryRemainsImmutableAndReadsDoNotAdvanceApplicationVersion(t *testing.T) {
	store, replica := newStore(t)
	first := checkpoint("run")
	if err := store.Create(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Payload = []byte(`{"step":"second"}`)
	if err := store.Save(t.Context(), second, first.Version); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewLease(t.Context(), workflow.CheckpointLease{RunID: first.RunID, OwnerID: first.OwnerID, ClaimSequence: first.ClaimSequence, ExpiresAt: first.LeaseExpiresAt.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	version, _ := replica.book.ReplicaVersion()
	calls := replica.calls
	history, err := store.ListCheckpoints(t.Context(), workflow.CheckpointHistoryRequest{RunID: first.RunID})
	if err != nil || len(history) != 2 || string(history[0].Payload) != string(first.Payload) || string(history[1].Payload) != string(second.Payload) {
		t.Fatalf("checkpoint versions lost: %+v %v", history, err)
	}
	if !history[1].LeaseExpiresAt.Equal(first.LeaseExpiresAt) {
		t.Fatal("renewal rewrote historical snapshot")
	}
	latest, err := store.Load(t.Context(), first.RunID)
	if err != nil || !latest.LeaseExpiresAt.Equal(first.LeaseExpiresAt.Add(time.Hour)) {
		t.Fatalf("current lease renewal lost: %+v %v", latest, err)
	}
	if _, err := store.List(t.Context(), runlog.Query{}); err != nil {
		t.Fatal(err)
	}
	after, _ := replica.book.ReplicaVersion()
	if after != version || replica.calls != calls {
		t.Fatal("read-only store queries appended application commands")
	}
}

func TestRunLogPreservesGlobalAppendOrderAndConcurrentIdempotence(t *testing.T) {
	store, replica := newStore(t)
	want := []runlog.Record{event("run-a", 3), event("run-b", 1), event("run-a", 1)}
	for _, record := range want {
		if err := store.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			if err := New(replica.book).Append(t.Context(), want[0]); err != nil {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	all, err := store.List(t.Context(), runlog.Query{SessionID: "session"})
	if err != nil || !reflect.DeepEqual(all, want) {
		t.Fatalf("append order/idempotence changed: %+v %v", all, err)
	}
	filtered, err := store.List(t.Context(), runlog.Query{RunID: "run-a", After: 1, Limit: 1})
	if err != nil || len(filtered) != 1 || filtered[0].Sequence != 3 {
		t.Fatalf("run filter=%+v %v", filtered, err)
	}
}

func (r *testReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	v, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}
func (r *testReplicator) Propose(_ context.Context, command ledger.ReplicatedWrite) ([]byte, error) {
	r.calls++
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(command.ID, command.ExpectedVersion+1, command.Payload)
}

func newStore(t *testing.T) (*Store, *testReplicator) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	replica := &testReplicator{book: book}
	if err := book.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	return New(book), replica
}

func TestStoreMatchesGopactRecoveryAndFencingContract(t *testing.T) {
	gopacttest.RequireStoreConformance(t, func(t *testing.T) workflow.Store { store, _ := newStore(t); return store })
}

func TestPlanWorkflowResumesInNewLedgerWithoutRepeatingCompletedStep(t *testing.T) {
	store, replica := newStore(t)
	var firstCalls, secondCalls atomic.Int64
	var pause atomic.Bool
	pause.Store(true)
	build := func(store workflow.Store) *workflow.Workflow[string, string] {
		flow := workflow.New[string, string]("plan-two-steps", workflow.WithStore(store), workflow.WithTopologyVersion("stable-plan-v1"))
		first := flow.Node("first", func(_ context.Context, text string) (string, error) { firstCalls.Add(1); return text + "-first", nil })
		second := flow.Node("second", func(_ context.Context, text string) (string, error) { secondCalls.Add(1); return text + "-second", nil })
		second.Guard(workflow.BeforeRun("approval", workflow.GuardFunc[string, string](func(context.Context, workflow.GuardContext[string, string]) (workflow.GuardDecision[string, string], error) {
			if pause.Swap(false) {
				return workflow.GuardInterrupt[string, string]{Request: workflow.InterruptRequest{ID: "approval"}}, nil
			}
			return workflow.GuardAllow[string, string]{}, nil
		})))
		flow.Entry(first)
		flow.Edge(first, second)
		flow.Exit(second)
		return flow
	}
	_, err := build(store).Invoke(t.Context(), "original", gopact.WithRunID("plan-original"))
	var interrupted workflow.InterruptError
	if !errors.As(err, &interrupted) || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("workflow did not stop between steps: %v calls=%d/%d", err, firstCalls.Load(), secondCalls.Load())
	}
	snapshot, err := replica.book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if err := restored.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := restored.AttachReplication(&testReplicator{book: restored}); err != nil {
		t.Fatal(err)
	}
	resumed := New(restored)
	output, err := build(resumed).Invoke(t.Context(), "", workflow.WithResume(workflow.ResumeRequest{RunID: "plan-original", CheckpointID: interrupted.CheckpointID, Resolutions: []workflow.InterruptResolution{{InterruptID: "approval", PayloadRef: "resolution://continue"}}}))
	if err != nil || output != "original-first-second" || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("resume repeated or lost completed work: %q %v calls=%d/%d", output, err, firstCalls.Load(), secondCalls.Load())
	}
	final, err := resumed.Load(t.Context(), "plan-original")
	if err != nil || final.Status != workflow.CheckpointCompleted {
		t.Fatalf("final checkpoint: %+v %v", final, err)
	}
}
