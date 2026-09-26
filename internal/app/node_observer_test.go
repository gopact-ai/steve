package app

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// startWorker serves one real node on loopback until the returned stop.
func startWorker(t *testing.T) (addr string, stop func()) {
	t.Helper()
	server := node.NewServer(node.ServerConfig{
		Name: "worker", Listen: "127.0.0.1:0", Token: "arrival-test",
		StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-served
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(10 * time.Second)
	for server.Addr() == "" || server.Addr() == "127.0.0.1:0" {
		if time.Now().After(deadline) {
			t.Fatal("the node did not listen")
		}
		time.Sleep(time.Millisecond)
	}
	return server.Addr(), stop
}

// Observers hear a machine's arrival late when history is slow to write. By
// then the connection it announced may be gone, or replaced by a newer
// one: it is still history, but skills shipped and worktrees swept for it
// would be for a connection that no longer exists.
func TestArrivalHeardAfterItsConnectionEndedSetsNothingGoing(t *testing.T) {
	addr, stopWorker := startWorker(t)
	nodes := node.NewRegistry("hub", map[string]node.Config{"worker": {Addr: addr, Token: "arrival-test"}})
	t.Cleanup(nodes.Close)
	lost := make(chan struct{})
	nodes.SetObserver(func(s node.Status, _ time.Time) {
		if !s.Up {
			close(lost)
		}
	})
	if _, err := nodes.Advert(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	var arrival node.Status
	for _, s := range nodes.Statuses() {
		if s.Name == "worker" {
			arrival = s
		}
	}
	if !arrival.Up {
		t.Fatalf("worker is not connected: %+v", arrival)
	}

	var recorded []string
	var setUp []int64
	observe := nodeObserver(nodes, func(_ time.Time, kind, _, _ string, _ map[string]string) { recorded = append(recorded, kind) },
		func(s node.Status) { setUp = append(setUp, s.Generation) })
	earlier := arrival
	earlier.Generation--
	observe(earlier, time.Now())
	observe(arrival, time.Now())
	stopWorker()
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("the registry never noticed the worker leave")
	}
	observe(arrival, time.Now())

	if len(recorded) != 3 || recorded[0] != "node.up" || recorded[1] != "node.up" || recorded[2] != "node.up" {
		t.Fatalf("history recorded %v, want every arrival heard", recorded)
	}
	if len(setUp) != 1 || setUp[0] != arrival.Generation {
		t.Fatalf("set up connections %v, want only the current connection %d", setUp, arrival.Generation)
	}
}

// While history is slow to write, a machine's loss waits in the queue and
// the registry has already acted on it: an attempt on that machine fails
// and the ledger says so. The history page must still show the loss first.
func TestLossHeardLateStaysBeforeTheFailureItCaused(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	view := readmodel.New(readmodel.Sources{Ledger: readmodel.Ledger{Book: book}, Observations: readmodel.Observations{Book: book}})
	addr, stopWorker := startWorker(t)
	nodes := node.NewRegistry("hub", map[string]node.Config{"worker": {Addr: addr, Token: "arrival-test"}})
	t.Cleanup(nodes.Close)
	slow := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-slow:
		default:
			close(slow)
		}
	})
	writing := make(chan struct{})
	lossRecorded := make(chan struct{})
	record := func(at time.Time, kind, subject, text string, data map[string]string) {
		if kind == "node.up" {
			close(writing)
			<-slow
		}
		view.ObserveAt(at, kind, subject, text, data)
		if kind == "node.down" {
			close(lossRecorded)
		}
	}
	nodes.SetObserver(nodeObserver(nodes, record, func(node.Status) {}))
	if _, err := nodes.Advert(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writing:
	case <-time.After(10 * time.Second):
		t.Fatal("the arrival was never recorded")
	}
	stopWorker()
	deadline := time.Now().Add(10 * time.Second)
	for stillConnected(nodes, node.Status{Name: "worker", Generation: 1}) {
		if time.Now().After(deadline) {
			t.Fatal("the registry never noticed the worker leave")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := book.Begin(t.Context(), "att-on-worker", "attempt", "failed", "test", nil); err != nil {
		t.Fatal(err)
	}
	close(slow)
	select {
	case <-lossRecorded:
	case <-time.After(10 * time.Second):
		t.Fatal("the loss was never recorded")
	}

	entries, _, err := view.History(t.Context(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	loss, failure := -1, -1
	for i, e := range entries {
		switch {
		case e.Kind == "observe.node.down":
			loss = i
		case e.Operation == "att-on-worker":
			failure = i
		}
	}
	// History is newest first.
	if loss < 0 || failure < 0 || failure > loss {
		t.Fatalf("history shows the failure at %d and the loss at %d, newest first; want the loss before the failure", failure, loss)
	}
}
