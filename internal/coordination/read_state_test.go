package coordination

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// On a leader that has committed an entry of its term, a quorum read is
// confirmed by a majority's answer and appends nothing to the log; one that
// has not yet appends one barrier first, as a read index request does. A
// follower still refuses the read.
func TestReadStateOnAnEstablishedLeaderAppendsNothing(t *testing.T) {
	for _, voters := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d-voters", voters), func(t *testing.T) {
			c := newTestCluster(t, voters)
			leader := c.leader()
			c.mu.RLock()
			for id, n := range c.nodes {
				if n == leader {
					continue
				}
				before := n.LastIndex()
				if _, err := n.ReadState(t.Context()); !errors.Is(err, ErrNotLeader) {
					t.Errorf("follower %s answered a quorum read: %v", id, err)
				}
				if n.LastIndex() != before {
					t.Errorf("a quorum read refused by follower %s grew its log", id)
				}
			}
			c.mu.RUnlock()
			leader.established.Store(0)
			before := leader.LastIndex()
			if _, err := leader.ReadState(t.Context()); err != nil {
				t.Fatal(err)
			}
			if grew := leader.LastIndex() - before; grew != 1 {
				t.Fatalf("a quorum read on a leader establishing its term appended %d entries, want one barrier", grew)
			}
			established := leader.LastIndex()
			for range 5 {
				if _, err := leader.ReadState(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if grew := leader.LastIndex() - established; grew != 0 {
				t.Fatalf("five quorum reads appended %d entries to an established leader's log, want none", grew)
			}
		})
	}
}

// A write on a leader that has committed an entry of its term appends the
// write's entry and nothing else; one that has not yet appends a barrier
// before it, once.
func TestAWriteOnAnEstablishedLeaderAppendsOneEntry(t *testing.T) {
	c := newTestCluster(t, 3, func(_ string, dir string) Application { return openCounter(t, dir) })
	leader := c.leader()
	write := func(version uint64) {
		t.Helper()
		if _, err := leader.ApplyApp(t.Context(), AppCommand{ID: fmt.Sprintf("write-%d", version), CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: version, Payload: []byte("1")}); err != nil {
			t.Fatal(err)
		}
	}
	leader.established.Store(0)
	before := leader.LastIndex()
	write(0)
	if grew := leader.LastIndex() - before; grew != 2 {
		t.Fatalf("a write on a leader establishing its term appended %d entries, want a barrier and the write", grew)
	}
	for version := uint64(1); version <= 5; version++ {
		before = leader.LastIndex()
		write(version)
		if grew := leader.LastIndex() - before; grew != 1 {
			t.Fatalf("write %d on an established leader appended %d entries, want only its own", version, grew)
		}
	}
	before = leader.LastIndex()
	if _, err := leader.SetAutoFailover(t.Context(), PolicyRequest{ID: "policy", Actor: "owner", ExpectedRevision: leader.Status().Revision, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if grew := leader.LastIndex() - before; grew != 1 {
		t.Fatalf("a membership policy change on an established leader appended %d entries, want only its own", grew)
	}
}

// gatedCounter holds the application write whose payload is gate until
// release is closed, reporting on entered that it has started.
type gatedCounter struct {
	*durableCounter
	gate     string
	entered  chan struct{}
	released chan struct{}
}

func (g *gatedCounter) Apply(command AppliedCommand) ([]byte, error) {
	if string(command.Payload) == g.gate {
		close(g.entered)
		<-g.released
	}
	return g.durableCounter.Apply(command)
}

// A quorum read holds every entry committed before it, including one the
// leader's state machine is still applying: it waits for the state machine
// rather than read around the entry.
func TestReadStateWaitsForEntriesCommittedBeforeIt(t *testing.T) {
	gated := &gatedCounter{gate: "7", entered: make(chan struct{}), released: make(chan struct{})}
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		gated.durableCounter = openCounter(t, dir)
		return gated
	})
	leader := c.leader()
	released := false
	release := func() {
		if !released {
			released = true
			close(gated.released)
		}
	}
	t.Cleanup(release)
	if _, err := leader.ReadState(t.Context()); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := leader.ApplyApp(t.Context(), AppCommand{ID: "held", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: []byte(gated.gate)})
		written <- err
	}()
	select {
	case <-gated.entered:
	case err := <-written:
		t.Fatalf("the write returned %v before it was applied", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the write was not applied")
	}
	// The single voter committed the entry before handing it over.
	read := make(chan State, 1)
	failed := make(chan error, 1)
	go func() {
		state, err := leader.ReadState(t.Context())
		if err != nil {
			failed <- err
			return
		}
		read <- state
	}()
	select {
	case state := <-read:
		t.Fatalf("a quorum read returned application version %d while a committed write was being applied", state.AppVersion)
	case err := <-failed:
		t.Fatalf("a quorum read failed while a committed write was being applied: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case state := <-read:
		if state.AppVersion != 1 {
			t.Fatalf("a quorum read returned application version %d, want the committed write's 1", state.AppVersion)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("the quorum read did not return once the write was applied")
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}
