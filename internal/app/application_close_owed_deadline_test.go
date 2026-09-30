package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
)

type closeSettlementReplicator struct {
	book             *ledger.Ledger
	entered, release chan struct{}
	once             sync.Once
	writes, active   atomic.Int64
	reject           error
	deadline         time.Time
}

func (r *closeSettlementReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *closeSettlementReplicator) Propose(ctx context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.writes.Add(1)
	r.active.Add(1)
	defer r.active.Add(-1)
	if r.entered != nil {
		r.once.Do(func() { r.deadline, _ = ctx.Deadline(); close(r.entered) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.release:
			return nil, errors.New("settlement released after its deadline")
		}
	}
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

func TestOwedCloseSettlementHonorsCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "shutdown"} {
		for _, code := range []string{"", "conflict"} {
			t.Run(mode+"/"+code, func(t *testing.T) {
				f := newOwedCloseFixture(t)
				if code != "" {
					f.node.err = &node.SessionError{Code: code, Message: "different binding"}
				}
				r := &closeSettlementReplicator{book: f.book, entered: make(chan struct{}), release: make(chan struct{})}
				if err := f.book.AttachReplication(r); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 250*time.Millisecond)
				}
				defer cancel()
				done := make(chan error, 1)
				go func() {
					if mode == "shutdown" {
						runReconciler(ctx, "close sessions", f.closes.Reconcile)
						done <- nil
						return
					}
					done <- f.closes.Reconcile(ctx)
				}()
				select {
				case <-r.entered:
				case <-time.After(2 * time.Second):
					cancel()
					close(r.release)
					<-done
					t.Fatal("settlement never reached replication")
				}
				if mode != "deadline" && (r.deadline.IsZero() || time.Until(r.deadline) > 20*time.Second) {
					cancel()
					close(r.release)
					<-done
					t.Fatal("settlement has no reconciliation deadline")
				}
				if mode != "deadline" {
					cancel()
				}
				select {
				case err := <-done:
					if mode != "shutdown" && !errors.Is(err, ctx.Err()) {
						t.Errorf("settlement returned %v, want %v", err, ctx.Err())
					}
				case <-time.After(time.Second):
					close(r.release)
					<-done
					t.Fatal("cancelled pass still waits for settlement replication")
				}
				if active := r.active.Load(); active != 0 {
					close(r.release)
					t.Fatalf("pass returned with %d settlement writes still running", active)
				}
				if got := f.store.OwedCloses(); len(got) != 1 || got[0] != f.owed {
					t.Fatalf("cancelled settlement lost its obligation: %+v", got)
				}
				if !f.closes.mu.TryLock() {
					t.Fatal("completed pass kept its lock")
				}
				f.closes.mu.Unlock()
			})
		}
	}
}
