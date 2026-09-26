package coordination

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// A read index is confirmed by a majority's answer to the leader, not by an
// entry in the log: only a leader that has not yet committed an entry of
// its own term appends one, once.
func TestReadIndexConfirmsLeadershipWithoutGrowingTheLog(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	c.mu.RLock()
	for id, n := range c.nodes {
		if n == leader {
			continue
		}
		if _, err := n.ReadIndex(t.Context()); !errors.Is(err, ErrNotLeader) {
			t.Errorf("follower %s answered a read index request: %v", id, err)
		}
	}
	c.mu.RUnlock()
	leader.established.Store(0)
	before := leader.LastIndex()
	index, err := leader.ReadIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if grew := leader.LastIndex() - before; grew != 1 {
		t.Fatalf("a leader establishing its term appended %d entries, want one barrier", grew)
	}
	if index <= before {
		t.Fatalf("read index %d misses the barrier at %d that established the term", index, before+1)
	}
	established := leader.LastIndex()
	for range 3 {
		if index, err = leader.ReadIndex(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if grew := leader.LastIndex() - established; grew != 0 {
		t.Fatalf("read index requests appended %d entries to an established leader's log", grew)
	}
	if index < established {
		t.Fatalf("read index %d misses committed entry %d", index, established)
	}
}

// A leader cut off from its majority confirms no read index, not even
// while its lease still has it take itself for the leader: each is
// confirmed by a majority's answer to the request itself.
func TestReadIndexNeedsAMajority(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	if _, err := leader.ReadIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Raft stops on the followers at once; their transports stay open and
	// hold the leader's requests unanswered. Answers they sent before, still
	// on their way, may count toward a confirmation: the leader is asked
	// once those have arrived, two heartbeat intervals later, and still
	// within its lease.
	var stopped sync.WaitGroup
	c.mu.RLock()
	for _, n := range c.nodes {
		if n != leader {
			stopped.Go(func() { n.raft.Shutdown().Error() })
		}
	}
	c.mu.RUnlock()
	stopped.Wait()
	time.Sleep(2 * leader.config.RaftConfig.HeartbeatTimeout / 10)
	if leader.raft.State() != raft.Leader {
		t.Fatalf("the leader stepped down before it was asked; the test asks within its lease of %s", leader.config.RaftConfig.LeaderLeaseTimeout)
	}
	ctx, cancel := context.WithTimeout(t.Context(), leader.config.ApplyTimeout)
	defer cancel()
	if index, err := leader.ReadIndex(ctx); err == nil {
		t.Fatalf("a leader without a majority confirmed read index %d", index)
	}
}

// Read index requests that reach a leader together before its term is
// established append one barrier between them, not one each.
func TestConcurrentReadIndexesEstablishATermWithOneBarrier(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	leader.established.Store(0)
	before := leader.LastIndex()
	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := leader.ReadIndex(t.Context())
			errs <- err
		}()
	}
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if grew := leader.LastIndex() - before; grew != 1 {
		t.Fatalf("%d read index requests establishing a term together appended %d entries, want one barrier", callers, grew)
	}
}

// A replica's state machine holds the barriers and the entry each term
// starts with once it has applied the commands before them, although
// none of them reaches it; it does not hold an index past its commit
// index.
func TestStateHoldsEntriesThatNeverReachTheStateMachine(t *testing.T) {
	c := newTestCluster(t, 1)
	leader := c.leader()
	for range 3 {
		if err := leader.barrier(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	last := leader.LastIndex()
	if applied := leader.Status().AppliedIndex; applied >= last {
		t.Fatalf("the state machine applied %d, want it short of the barrier at %d", applied, last)
	}
	if !leader.StateHolds(last) {
		t.Fatalf("the state machine does not hold barrier %d it has passed", last)
	}
	if leader.StateHolds(leader.raft.CommitIndex() + 1) {
		t.Fatal("the state machine holds an index past the commit index")
	}
}
