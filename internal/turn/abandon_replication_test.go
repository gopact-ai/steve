package turn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestAbandonmentAndAccountingBecomeVisibleOnlyAfterTheSameCommit(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "reject"}[rejected], func(t *testing.T) {
			c, tasks, r := abandonFixture(t)
			book := ledgerOf(t, c)
			replica := &forceControlReplication{book: book, entered: make(chan struct{}), release: make(chan struct{})}
			if rejected {
				replica.reject = errors.New("no quorum")
			}
			if err := book.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := NewAbandonControl(c).AbandonAttempt(ctx, r.ID, "owner", 1); done <- err }()
			select {
			case <-replica.entered:
			case err := <-done:
				t.Fatalf("request returned before proposing: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var row task.Attempt
			readErr := book.Read(ctx, func(tx *ledger.ReadTx) error {
				var err error
				row, _, _, err = task.ReadAccountingTx(tx, r.TaskID, r.ID, r.TurnID)
				return err
			})
			before, recordErr := c.attempts.Get(ctx, r.ID)
			close(replica.release)
			err := <-done
			if readErr != nil || recordErr != nil {
				t.Fatalf("read failed: %v %v", readErr, recordErr)
			}
			if !row.AccountingFrozenAt.IsZero() || before.Abandoned != nil {
				t.Fatal("uncommitted abandonment or accounting became visible")
			}
			if rejected && !errors.Is(err, replica.reject) || !rejected && err != nil {
				t.Fatalf("commit result=%v", err)
			}
			loaded, loadErr := task.OpenLedger(book)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			for _, store := range []*task.Store{tasks, loaded} {
				tracked, _ := store.Get(r.TaskID)
				if tracked.Attempts[0].AccountingFrozenAt.IsZero() != rejected {
					t.Fatal("task cache and ledger disagree on the accepted cutoff")
				}
			}
			current, _ := attempt.New(book).Get(ctx, r.ID)
			if (current.Abandoned == nil) != rejected {
				t.Fatal("execution and accounting did not commit together")
			}
		})
	}
}
