package task

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestResumeRejectsChangedTaskWithoutOverridingStop(t *testing.T) {
	for _, stop := range []State{StatePaused, StateCancelled} {
		t.Run(string(stop), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			store, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			tracked, err := store.Create(Task{Channel: "chat", Transport: "console", Member: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			tracked, err = store.Advance(tracked.ID, StatePaused)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetAside(tracked.ID, stop); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Get(tracked.ID)
			if _, err := store.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, ResumeAdmission{}); !errors.Is(err, ErrExecutionStopped) {
				t.Fatalf("stale resume did not preserve revocation: %v", err)
			}
			if after, _ := store.Get(tracked.ID); !reflect.DeepEqual(before, after) {
				t.Fatal("stale resume overwrote a newer stop")
			}
		})
	}
}
