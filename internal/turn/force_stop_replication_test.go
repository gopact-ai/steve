package turn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

type forceControlReplication struct {
	book    *ledger.Ledger
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	reject  error
}

func (r *forceControlReplication) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	v, e := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, e
}
func (r *forceControlReplication) Propose(ctx context.Context, w ledger.ReplicatedWrite) ([]byte, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
	}
	if r.reject != nil {
		return nil, r.reject
	}
	return r.book.ApplyReplicated(w.ID, w.ExpectedVersion+1, w.Payload)
}

func TestForceStopRevocationCannotOutrunItsReplicatedIntent(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "rejected"}[rejected], func(t *testing.T) {
			c, tasks, r, proof := forceStopControlFixture(t)
			book := ledgerOf(t, c)
			replica := &forceControlReplication{book: book, entered: make(chan struct{}), release: make(chan struct{})}
			if rejected {
				replica.reject = errors.New("no quorum")
			}
			if err := book.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- NewForceStopControl(c).ForceStopAttempt(ctx, r.ID, "owner", 0) }()
			select {
			case <-replica.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var before task.Task
			if err := book.Read(ctx, func(tx *ledger.ReadTx) error { var err error; before, _, err = task.GetTx(tx, r.TaskID); return err }); err != nil {
				close(replica.release)
				<-result
				t.Fatal(err)
			}
			pending, _ := c.attempts.Get(ctx, r.ID)
			if before.State != task.StateRunning || pending.ForceStop != nil {
				close(replica.release)
				<-result
				t.Fatal("uncommitted control became visible")
			}
			close(replica.release)
			err := <-result
			if rejected {
				if !errors.Is(err, replica.reject) {
					t.Fatalf("refused control=%v", err)
				}
				stored, _ := tasks.Get(r.TaskID)
				got, _ := c.attempts.Get(ctx, r.ID)
				if stored.State != task.StateRunning || got.ForceStop != nil {
					t.Fatal("refused proposal partially installed control")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// A normal cancellation result may arrive at any time, but after this
			// commit it cannot be mistaken for the required physical process exit.
			proof.ObservedAt = time.Now()
			if _, err := c.attempts.ConfirmTaskStopped(ctx, r.ID, "in-flight-cancel", proof); err == nil {
				t.Fatal("old command result retired atomic force intent")
			}
			restored, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			tracked, _ := restored.Get(r.TaskID)
			got, err := attempt.New(book).Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tracked.State != task.StateCancelled || got.ForceStop == nil || got.ForceStop.Level != "kill" || !got.Unsettled {
				t.Fatal("restart lost the atomic control")
			}
		})
	}
}
