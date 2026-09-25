package app

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node"
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
