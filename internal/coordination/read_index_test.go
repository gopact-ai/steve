package coordination

import (
	"context"
	"errors"
	"testing"
	"time"
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

// A leader cut off from its majority stops confirming read indexes within
// its lease.
func TestReadIndexNeedsAMajority(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	if _, err := leader.ReadIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	var followers []string
	for id, n := range c.nodes {
		if n != leader {
			followers = append(followers, id)
		}
	}
	c.mu.RUnlock()
	for _, id := range followers {
		c.stop(id)
	}
	stopped := time.Now()
	bound := leader.config.RaftConfig.LeaderLeaseTimeout + leader.config.ApplyTimeout
	for {
		ctx, cancel := context.WithTimeout(t.Context(), leader.config.ApplyTimeout)
		index, err := leader.ReadIndex(ctx)
		cancel()
		if err != nil {
			return
		}
		if time.Since(stopped) > bound {
			t.Fatalf("a leader without a majority still confirmed read index %d %s after losing it", index, bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
