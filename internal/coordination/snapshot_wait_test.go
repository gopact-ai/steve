package coordination

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stalledSnapshots holds every application snapshot until release. Raft takes
// the application's bytes on its FSM goroutine, so a held snapshot also holds
// every later snapshot request and the replica's applies.
type stalledSnapshots struct {
	Application
	taken   atomic.Int32
	release chan struct{}
	once    sync.Once
}

func (a *stalledSnapshots) Snapshot() ([]byte, error) {
	a.taken.Add(1)
	<-a.release
	return a.Application.Snapshot()
}

func (a *stalledSnapshots) resume() { a.once.Do(func() { close(a.release) }) }

// stalledSnapshotCluster is a one-node cluster whose application snapshots are
// held once the cluster has started.
func stalledSnapshotCluster(t *testing.T) (*testCluster, *stalledSnapshots) {
	t.Helper()
	var app *stalledSnapshots
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		app = &stalledSnapshots{Application: openCounter(t, dir), release: make(chan struct{})}
		return app
	})
	// Registered after the cluster, so it runs before the nodes are closed.
	t.Cleanup(app.resume)
	return c, app
}

func within(t *testing.T, d time.Duration, what string, call func() error) error {
	t.Helper()
	finished := make(chan error, 1)
	go func() { finished <- call() }()
	select {
	case err := <-finished:
		return err
	case <-time.After(d):
		t.Fatalf("%s still waiting after %s", what, d)
		return nil
	}
}

// A replica taking in an application must install the leader's baseline
// snapshot, so Join takes one while holding membershipMu. Raft cannot
// withdraw a snapshot request, and one held up, here by the application,
// must not keep Join, and every membership change queued behind it, waiting.
func TestJoinGivesUpOnABaselineSnapshotThatDoesNotFinish(t *testing.T) {
	c, app := stalledSnapshotCluster(t)
	leader := c.leader()
	peer := addUnjoinedTestReplica(t, c, "joining-domain")
	request := JoinRequest{ID: "join-stalled", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}}
	applyTimeout := leader.config.ApplyTimeout
	err := within(t, 3*applyTimeout, "join behind a snapshot that does not finish", func() error {
		_, err := leader.Join(context.Background(), request)
		return err
	})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "snapshot of the application baseline for new-node did not finish") {
		t.Fatalf("join behind a snapshot that did not finish returned %v", err)
	}
	app.resume()
	if _, err := leader.Join(t.Context(), request); err != nil {
		t.Fatalf("the same join failed once snapshots could finish: %v", err)
	}
	if _, ok := leader.Status().Members["new-node"]; !ok {
		t.Fatal("new-node did not join")
	}
}

// Snapshot is bounded the same way. Callers that give up leave at most one
// snapshot request behind, which later callers share instead of queueing
// another behind it.
func TestSnapshotGivesUpAndLeavesOneRequestBehind(t *testing.T) {
	c, app := stalledSnapshotCluster(t)
	node := c.leader()
	applyTimeout := node.config.ApplyTimeout
	for i := 0; i < 2; i++ {
		err := within(t, 3*applyTimeout, "snapshot that does not finish", func() error { return node.Snapshot(context.Background()) })
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "snapshot of the replica did not finish") {
			t.Fatalf("snapshot that did not finish returned %v", err)
		}
	}
	// A caller whose own deadline ends first gets that deadline back.
	short, cancel := context.WithTimeout(context.Background(), applyTimeout/3)
	defer cancel()
	err := within(t, 3*applyTimeout, "snapshot with a short deadline", func() error { return node.Snapshot(short) })
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("snapshot whose caller's deadline ended first returned %v", err)
	}
	app.resume()
	if err := node.Snapshot(t.Context()); err != nil {
		t.Fatalf("snapshot failed once the application could take one: %v", err)
	}
	// Raft serves snapshot requests one at a time and in order, so every
	// request left behind has been served by now.
	if taken := app.taken.Load(); taken != 2 {
		t.Fatalf("the application took %d snapshots; want the one both callers gave up on and the last", taken)
	}
}
