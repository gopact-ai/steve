package contentreplica

import (
	"errors"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestUploadAllocationClockAndPendingIntentCommitTogether(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ref := checkpoint.Reference([]byte("pending intent"))
	object := Object{Scope: Scope{ProjectID: "p", Level: "internal", HomeNodeID: "a"}, Kind: Material, Key: ref.SHA256, Blob: ref}
	rejected := errors.New("owner transaction rejected")
	err = book.Update(t.Context(), func(tx *ledger.Tx) error {
		u, err := allocateUpload(tx, object, []string{"a"})
		if err != nil || uploadSequence(u.ID) != 1 {
			t.Fatalf("first allocation: %+v %v", u, err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	var rows int
	if err := book.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind LIKE 'content-%'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rolled-back intent advanced durable allocator: %d %v", rows, err)
	}
	const count = 16
	var wg sync.WaitGroup
	results, failures := make(chan Upload, count), make(chan error, count)
	for range count {
		wg.Go(func() {
			var u Upload
			err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				var err error
				u, err = allocateUpload(tx, object, []string{"a"})
				return err
			})
			results <- u
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	for u := range results {
		n := uploadSequence(u.ID)
		if n == 0 || n > count || seen[n] {
			t.Fatalf("concurrent transaction reused allocation order: %s", u.ID)
		}
		seen[n] = true
		var pending uploadRecord
		if found, err := book.GetBinding(t.Context(), uploadKind, u.ID, &pending); err != nil || !found || pending.Upload != u || pending.State != "pending" {
			t.Fatalf("sequence committed without its exact pending intent: %+v %v", pending, err)
		}
	}
	var clock uploadClock
	if found, err := book.GetBinding(t.Context(), uploadClockKind, "sequence", &clock); err != nil || !found || clock.High != count {
		t.Fatalf("clock=%+v found=%v err=%v", clock, found, err)
	}
}
