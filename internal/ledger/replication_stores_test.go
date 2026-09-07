package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type storeReplicator struct {
	book   *ledger.Ledger
	reject error
}

func (r *storeReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *storeReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func TestAttemptCompletionPreservesItsAtomicBoundaryWithReplication(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	r := &storeReplicator{book: book}
	if err := book.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	s := attempt.New(book)
	ctx := t.Context()
	record, err := s.Open(ctx, attempt.Spec{ID: "work", Kind: attempt.KindStep, Project: "p", Workspace: project.Workspace{ID: "wt-work", Project: "p", Kind: project.KindWorktree}, Scope: attempt.ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running, attempt.BindReady} {
		record, err = s.Advance(ctx, record.ID, state, "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSessionSettled(ctx, record.ID, "worker exited"); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("quorum unavailable")
	r.reject = rejected
	completion := attempt.Completion{Result: attempt.Result{Artifact: "artifact", Summary: "done"}, Usage: &attempt.Usage{Input: 100, Output: 20, Reported: true}, Binding: &attempt.NameBinding{Name: "result/work"}}
	if _, err := s.Complete(ctx, record.ID, "worker", completion); !errors.Is(err, rejected) {
		t.Fatalf("completion rejection=%v", err)
	}
	got, err := s.Get(ctx, record.ID)
	if err != nil || got.State != attempt.BindReady || got.Result != nil || got.Usage != nil || !got.EndedAt.IsZero() {
		t.Fatalf("partial completion escaped: %+v %v", got, err)
	}
	if _, ok, err := book.Name(ctx, "result/work"); err != nil || ok {
		t.Fatalf("name escaped before commit: %v %v", ok, err)
	}
	r.reject = nil
	got, err = s.Complete(ctx, record.ID, "worker", completion)
	if err != nil {
		t.Fatal(err)
	}
	name, ok, err := book.Name(ctx, "result/work")
	if err != nil || !ok || name.Artifact != "artifact" || got.State != attempt.Bound || got.Usage.Input != 100 || got.Result.Summary != "done" {
		t.Fatalf("completion incomplete: %+v %+v %v", got, name, err)
	}
	for _, lease := range record.Leases {
		if err := book.Check(ctx, lease); !errors.Is(err, ledger.ErrStale) {
			t.Fatalf("completion left live lease: %v", err)
		}
	}
}
