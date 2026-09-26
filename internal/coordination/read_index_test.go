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
