package readmodel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

// payloadReplicator applies each write to the same ledger, as a one-member
// cluster would, and remembers how many bytes every write replicated.
type payloadReplicator struct {
	book *ledger.Ledger
	// before runs ahead of apply; its error rejects the write. after runs
	// once the write has applied; its error reports an unknown outcome
	// for a write that did commit.
	before, after func() error
	// unavailable runs as a write starts, where the coordinator checks it
	// may write at all; its error fails the write before anything is
	// proposed, as a replica that cannot catch up does.
	unavailable func() error

	mu    sync.Mutex
	sizes []int
}

func (r *payloadReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	r.mu.Lock()
	unavailable := r.unavailable
	r.mu.Unlock()
	if unavailable != nil {
		if err := unavailable(); err != nil {
			return ledger.ReplicaPosition{}, err
		}
	}
	v, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}

func (r *payloadReplicator) Propose(_ context.Context, w ledger.ReplicatedWrite) ([]byte, error) {
	r.mu.Lock()
	r.sizes = append(r.sizes, len(w.Payload))
	before, after := r.before, r.after
	r.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return nil, err
		}
	}
	out, err := r.book.ApplyReplicated(w.ID, w.ExpectedVersion+1, w.Payload)
	if err != nil {
		return nil, err
	}
	if after != nil {
		if err := after(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// replicated is the bytes replicated since the mark'th write.
func (r *payloadReplicator) replicated(mark int) (writes, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.sizes[mark:] {
		bytes += n
	}
	return len(r.sizes) - mark, bytes
}

func (r *payloadReplicator) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sizes)
}

func replicatedBook(t *testing.T, dir string) (*ledger.Ledger, *payloadReplicator) {
	t.Helper()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	r := &payloadReplicator{book: book}
	if err := book.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	return book, r
}

// observeNodeUp records one connection the way the hub does, with a
// realistically sized sentence and facts.
func observeNodeUp(m *Model, i int) {
	name := fmt.Sprintf("node-%04d", i)
	m.Observe("node.up", name, name+" connected: build-host.example linux/amd64, build v0.0.0-20260926-abcdef012345",
		map[string]string{"host": "build-host.example", "os": "linux", "arch": "amd64", "build": "v0.0.0-20260926-abcdef012345"})
}

// Recording one observation replicates about one observation's worth of
// bytes however much history is already kept: a hub whose links to its
// members are slow must not resend its whole history per connection fact.
func TestObserveReplicatesOneObservationWhateverTheHistory(t *testing.T) {
	book, r := replicatedBook(t, t.TempDir())
	m := New(Sources{Observations: ledgerObservationStore(book)})
	measure := func(i int) (int, int) {
		mark := r.mark()
		observeNodeUp(m, i)
		return r.replicated(mark)
	}
	for i := range 9 {
		observeNodeUp(m, i)
	}
	earlyWrites, early := measure(9)
	// Past the retention limit, so every write also forgets the oldest.
	for i := 10; i < observationsKept+20; i++ {
		observeNodeUp(m, i)
	}
	lateWrites, late := measure(observationsKept + 20)
	t.Logf("one observation replicated %d bytes with 9 kept, %d bytes with %d kept", early, late, observationsKept)
	if earlyWrites != 1 || lateWrites != 1 {
		t.Fatalf("one observation made %d writes with 9 kept and %d with %d kept, want one each", earlyWrites, lateWrites, observationsKept)
	}
	if late > early+512 {
		t.Fatalf("one observation replicated %d bytes with %d kept but %d bytes with 9 kept: the write grows with the retained history", late, observationsKept, early)
	}
}

// A hub that could not write for a while catches up in writes no larger
// than a few dozen observations, oldest first: one write carrying the
// whole backlog is the resend of the full history that saturated slow
// member links. More failed observations than are kept lose the oldest,
// and once caught up the ledger holds exactly the newest in order.
func TestObserveCatchesUpAfterFailuresInBoundedWrites(t *testing.T) {
	const maxWrite = 32 << 10
	book, r := replicatedBook(t, t.TempDir())
	m := New(Sources{Observations: ledgerObservationStore(book)})
	n := 0
	for ; n < observationsKept+5; n++ {
		observeNodeUp(m, n)
	}
	r.unavailable = func() error { return errors.New("replica cannot catch up") }
	for range observationsKept + 100 {
		observeNodeUp(m, n)
		n++
	}
	r.unavailable = nil
	mark := r.mark()
	store := ledgerObservationStore(book)
	for caughtUp := false; !caughtUp; {
		if r.mark()-mark > observationsKept {
			t.Fatalf("still behind after %d writes", r.mark()-mark)
		}
		observeNodeUp(m, n)
		n++
		caughtUp = reflect.DeepEqual(restoredObservations(t, store), m.observations)
	}
	r.mu.Lock()
	sizes := append([]int(nil), r.sizes[mark:]...)
	r.mu.Unlock()
	for i, size := range sizes {
		if size > maxWrite {
			t.Fatalf("catch-up write %d of %d replicated %d bytes, want at most %d", i+1, len(sizes), size, maxWrite)
		}
	}
	got := subjects(restoredObservations(t, store))
	first, last := fmt.Sprintf("node-%04d", n-observationsKept), fmt.Sprintf("node-%04d", n-1)
	if len(got) != observationsKept || got[0] != first || got[len(got)-1] != last {
		t.Fatalf("caught up to %d observations, want the newest %d, %s…%s", len(got), observationsKept, first, last)
	}
	t.Logf("caught up in %d writes, the largest %d bytes", len(sizes), slices.Max(sizes))
}
