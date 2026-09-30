package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type owedCloseBatchStore interface {
	SettleOwedCloses(context.Context, ...OwedClose) error
	OwedClosesContext(context.Context) ([]OwedClose, error)
}

func TestOwedCloseBatchKeepsReplacementsAndOtherCloses(t *testing.T) {
	store, book, replica := replicatedState(t)
	stale := owedSession(t, store, "one", "agent", "ns_one")
	if err := store.ArchiveSessionOwingClose("one", "agent", stale.OwedAt, stale); err != nil {
		t.Fatal(err)
	}
	replacement := owedSession(t, store, "one", "agent", "ns_one")
	replacement.AttemptID = "new-attempt"
	if err := store.ArchiveSessionOwingClose("one", "agent", replacement.OwedAt, replacement); err != nil {
		t.Fatal(err)
	}
	current := owedSession(t, store, "two", "agent", "ns_two")
	if err := store.ArchiveSessionOwingClose("two", "agent", current.OwedAt, current); err != nil {
		t.Fatal(err)
	}
	unrelated := owedSession(t, store, "three", "agent", "ns_three")
	if err := store.ArchiveSessionOwingClose("three", "agent", unrelated.OwedAt, unrelated); err != nil {
		t.Fatal(err)
	}
	batch, ok := any(store).(owedCloseBatchStore)
	if !ok {
		t.Fatal("store has no bounded close batch")
	}
	before := store.OwedCloses()
	writes := len(replica.payloads)
	replica.reject = errors.New("no quorum")
	if err := batch.SettleOwedCloses(t.Context(), stale, current); !errors.Is(err, replica.reject) {
		t.Fatalf("refused batch = %v", err)
	}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused batch changed memory: %+v", got)
	}
	replica.reject = nil
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.OwedCloses(); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused batch changed disk: %+v", got)
	}
	if err := batch.SettleOwedCloses(t.Context(), stale, current); err != nil {
		t.Fatal(err)
	}
	want := []OwedClose{replacement, unrelated}
	if got := store.OwedCloses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("batch lost a replacement or unrelated close: %+v", got)
	}
	if got := len(replica.payloads) - writes; got != 2 {
		t.Fatalf("two batches proposed %d writes", got)
	}
	writes = len(replica.payloads)
	if err := batch.SettleOwedCloses(t.Context(), stale, current); err != nil {
		t.Fatal(err)
	}
	if len(replica.payloads) != writes {
		t.Fatal("repeated settlement wrote a new document")
	}
}

func TestOwedCloseBatchDoesNotWaitOnTheStateLock(t *testing.T) {
	store, _, _ := replicatedState(t)
	batch, ok := any(store).(owedCloseBatchStore)
	if !ok {
		t.Fatal("store has no bounded close batch")
	}
	for _, read := range []bool{false, true} {
		store.mu.Lock()
		done := make(chan error, 1)
		go func() {
			if read {
				_, err := batch.OwedClosesContext(t.Context())
				done <- err
			} else {
				done <- batch.SettleOwedCloses(t.Context(), OwedClose{UpstreamID: "ns_one"})
			}
		}()
		select {
		case err := <-done:
			store.mu.Unlock()
			if err == nil {
				t.Fatalf("busy state accepted batch/read=%v", read)
			}
		case <-time.After(time.Second):
			store.mu.Unlock()
			<-done
			t.Fatalf("batch/read=%v waited for state lock", read)
		}
	}
}

func TestCancelledOwedCloseBatchDoesNotWrite(t *testing.T) {
	store, _, replica := replicatedState(t)
	owed := owedSession(t, store, "one", "agent", "ns_one")
	if err := store.ArchiveSessionOwingClose("one", "agent", owed.OwedAt, owed); err != nil {
		t.Fatal(err)
	}
	batch, ok := any(store).(owedCloseBatchStore)
	if !ok {
		t.Fatal("store has no bounded close batch")
	}
	before := len(replica.payloads)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := batch.SettleOwedCloses(ctx, owed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch = %v", err)
	}
	if _, err := batch.OwedClosesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot = %v", err)
	}
	if len(replica.payloads) != before || !reflect.DeepEqual(store.OwedCloses(), []OwedClose{owed}) {
		t.Fatal("cancelled batch changed state")
	}
}
