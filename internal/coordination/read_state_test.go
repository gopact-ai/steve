package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
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
				if _, err := n.ReadState(t.Context()); !errors.Is(err, ErrNotLeader) {
					t.Errorf("follower %s answered a quorum read: %v", id, err)
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

// A write on a leader that has committed an entry of its term, to the
// application or to membership, appends the write's entry and nothing
// else; one that has not yet appends a barrier before it, once.
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
	if _, err := leader.Rename(t.Context(), RenameRequest{ID: "rename", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: "node-2", Name: "builder"}); err != nil {
		t.Fatal(err)
	}
	if grew := leader.LastIndex() - before; grew != 1 {
		t.Fatalf("renaming a member on an established leader appended %d entries, want only its own", grew)
	}
}

// snapshotGate holds the next application snapshot until release is
// closed, reporting on entered that it has started. Raft takes snapshots
// on the goroutine that applies committed entries, so none is applied
// meanwhile, while the machine lets readers in.
type snapshotGate struct {
	*durableCounter
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
}

func (g *snapshotGate) Snapshot() ([]byte, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.released
	}
	return g.durableCounter.Snapshot()
}

// holdCommittedWrite holds the state machine of leader, a single voter,
// in a snapshot and commits a write behind it, which stays unapplied until
// release is called. release is also called when the test ends.
func holdCommittedWrite(t *testing.T, leader *Service, gate *snapshotGate) (release func(), write raft.ApplyFuture) {
	t.Helper()
	var once sync.Once
	release = func() { once.Do(func() { close(gate.released) }) }
	t.Cleanup(release)
	if _, err := leader.ReadState(t.Context()); err != nil {
		t.Fatal(err)
	}
	gate.armed.Store(true)
	go leader.Snapshot(t.Context())
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the snapshot did not start")
	}
	// Appended as submit appends a write, without what submit does first,
	// so that only the write is waiting for the state machine.
	request := AppCommand{ID: "held", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: []byte("7")}
	encoded, err := json.Marshal(command{Kind: "app", ID: request.ID, Actor: request.CallerNodeID, Fingerprint: fingerprint("app", request), App: request, ClusterID: leader.config.ClusterID, Time: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	before := leader.LastIndex()
	write = leader.raft.Apply(encoded, leader.config.ApplyTimeout)
	eventually(t, 10*time.Second, func() bool {
		for index := before + 1; index <= leader.raft.CommitIndex(); index++ {
			var entry raft.Log
			if leader.store.GetLog(index, &entry) == nil && entry.Type == raft.LogCommand {
				return true
			}
		}
		return false
	})
	return release, write
}

// A quorum read holds every entry committed before it, including one the
// leader's state machine has not yet applied: it waits for the state
// machine rather than read around the entry.
func TestReadStateWaitsForEntriesCommittedBeforeIt(t *testing.T) {
	gate := &snapshotGate{entered: make(chan struct{}), released: make(chan struct{})}
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		gate.durableCounter = openCounter(t, dir)
		return gate
	})
	leader := c.leader()
	release, write := holdCommittedWrite(t, leader, gate)
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
		t.Fatalf("a quorum read returned application version %d while a committed write waited to be applied", state.AppVersion)
	case err := <-failed:
		t.Fatalf("a quorum read failed while a committed write waited to be applied: %v", err)
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
	if err := write.Error(); err != nil {
		t.Fatal(err)
	}
}

// A quorum read that waits for the state machine looks at the log again
// when the state machine has applied more of it, not on a timer: while the
// state machine is held, the waiting read leaves the log store alone.
func TestReadStateWaitingForTheStateMachineDoesNotKeepReadingTheLog(t *testing.T) {
	gate := &snapshotGate{entered: make(chan struct{}), released: make(chan struct{})}
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		gate.durableCounter = openCounter(t, dir)
		return gate
	})
	leader := c.leader()
	release, write := holdCommittedWrite(t, leader, gate)
	read := make(chan error, 1)
	go func() {
		_, err := leader.ReadState(t.Context())
		read <- err
	}()
	// The read confirms its index and looks at the log once before it
	// waits; count only what it reads while it waits.
	time.Sleep(50 * time.Millisecond)
	before := leader.store.Stats().TxN
	time.Sleep(300 * time.Millisecond)
	transactions := leader.store.Stats().TxN - before
	select {
	case err := <-read:
		t.Fatalf("a quorum read returned while a committed write waited to be applied: %v", err)
	default:
	}
	if transactions > 10 {
		t.Errorf("a quorum read waiting for a held state machine started %d log store reads in 300ms", transactions)
	}
	release()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the quorum read did not return once the write was applied")
	}
	if err := write.Error(); err != nil {
		t.Fatal(err)
	}
}

// A leader that hands its leadership to another voter can be replaced by it
// without an election timeout: the target's vote request tells the voters to
// drop the leader they follow, so an answer to a heartbeat sent before the
// target took over no longer shows a majority behind the leader. As the
// barrier a write starts with, a quorum read and a read index request are
// refused while the transfer runs.
func TestQuorumReadsAreRefusedWhileTheLeaderTransfersLeadership(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	var others []string
	for id := range leader.Status().Voters {
		if id != leader.config.NodeID {
			others = append(others, id)
		}
	}
	sort.Strings(others)
	// Remove tries targets in ID order; the first one receives the transfer.
	target, coordinator := others[0], others[1]
	eventually(t, 5*time.Second, func() bool {
		_, err := leader.Transfer(t.Context(), TransferRequest{ID: "move-before-remove", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: coordinator})
		if errors.Is(err, ErrNotReady) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	})
	applied := leader.Status().AppliedIndex
	eventually(t, 5*time.Second, func() bool { return c.nodes[target].Status().AppliedIndex >= applied })
	// The target acknowledges no new entry, so the transfer keeps trying to
	// catch it up until the election timeout ends the attempt.
	if err := newRaftLoopPause(t, c.nodes[target]).pause(); err != nil {
		t.Fatal(err)
	}
	transferring := func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := leader.barrier(ctx)
		return errors.Is(err, ErrUnavailable) && strings.Contains(err.Error(), raft.ErrLeadershipTransferInProgress.Error())
	}
	// A transfer that ends between the reads and the check after them
	// leaves the attempt without a verdict; the next attempt starts another.
	for attempt := 0; attempt < 20; attempt++ {
		removed := make(chan error, 1)
		go func() {
			_, err := leader.Remove(t.Context(), RemoveRequest{ID: "remove-leader", Actor: "owner", NodeID: leader.config.NodeID})
			removed <- err
		}()
		started := time.Now()
		for !transferring() && time.Since(started) < 5*time.Second {
			time.Sleep(time.Millisecond)
		}
		_, readErr := leader.ReadState(t.Context())
		_, indexErr := leader.ReadIndex(t.Context())
		during := transferring()
		<-removed
		if !during {
			continue
		}
		if readErr == nil {
			t.Fatal("a quorum read was confirmed while the leader transferred its leadership")
		}
		if indexErr == nil {
			t.Fatal("a read index was confirmed while the leader transferred its leadership")
		}
		if !errors.Is(readErr, ErrUnavailable) || !errors.Is(indexErr, ErrUnavailable) {
			t.Fatalf("reads during a leadership transfer failed with %v and %v, want unavailable", readErr, indexErr)
		}
		return
	}
	t.Fatal("no read ran while a leadership transfer did")
}

// A quorum read gives up within ApplyTimeout, as the barrier it replaces
// did, although its caller sets no deadline and other reads wait with it
// for the leader to establish its term: a ledger writer holds its lock
// through one.
func TestQuorumReadsGiveUpWithinApplyTimeout(t *testing.T) {
	gate := &snapshotGate{entered: make(chan struct{}), released: make(chan struct{})}
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		gate.durableCounter = openCounter(t, dir)
		return gate
	})
	leader := c.leader()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate.released) }) })
	if _, err := leader.ReadState(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A barrier completes once the state machine has applied the entries
	// before it, which it cannot while it is held.
	gate.armed.Store(true)
	go leader.Snapshot(t.Context())
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the snapshot did not start")
	}
	leader.established.Store(0)
	applyTimeout := leader.config.ApplyTimeout
	took := make(chan time.Duration, 3)
	for range cap(took) {
		go func() {
			started := time.Now()
			if _, err := leader.ReadState(context.Background()); err == nil {
				t.Error("a quorum read succeeded while the state machine was held")
			}
			took <- time.Since(started)
		}()
	}
	for range cap(took) {
		if d := <-took; d > applyTimeout+applyTimeout/2 {
			t.Errorf("a quorum read gave up after %s, ApplyTimeout is %s", d.Round(time.Millisecond), applyTimeout)
		}
	}
}

// A quorum read that gives up on its own ApplyTimeout fails as unavailable
// and says what it waited for: here the barrier that establishes the
// leader's term, which one read appends and the others queue behind. A
// caller whose own deadline ends first gets that deadline back.
func TestQuorumReadsThatGiveUpSayWhatTheyWaitedFor(t *testing.T) {
	gate := &snapshotGate{entered: make(chan struct{}), released: make(chan struct{})}
	c := newTestCluster(t, 1, func(_ string, dir string) Application {
		gate.durableCounter = openCounter(t, dir)
		return gate
	})
	leader := c.leader()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate.released) }) })
	if _, err := leader.ReadState(t.Context()); err != nil {
		t.Fatal(err)
	}
	gate.armed.Store(true)
	go leader.Snapshot(t.Context())
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the snapshot did not start")
	}
	leader.established.Store(0)
	applyTimeout := leader.config.ApplyTimeout
	unbounded := make(chan error, 3)
	for range cap(unbounded) {
		go func() {
			_, err := leader.ReadState(context.Background())
			unbounded <- err
		}()
	}
	short, cancel := context.WithTimeout(t.Context(), applyTimeout/3)
	defer cancel()
	if _, err := leader.ReadState(short); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnavailable) {
		t.Errorf("a quorum read whose caller's deadline ended first failed with %v, not with that deadline", err)
	}
	for range cap(unbounded) {
		err := <-unbounded
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("a quorum read that gave up on ApplyTimeout failed with %v, not as unavailable", err)
			continue
		}
		if text := err.Error(); !strings.Contains(text, "term") || !strings.Contains(text, "barrier did not complete within "+applyTimeout.String()) || !strings.Contains(text, "applied") {
			t.Errorf("a quorum read that gave up does not say which barrier it waited for or how far the log got: %v", err)
		}
	}
}

// A leader whose followers fall silent keeps leading until its lease runs
// out. A quorum read it serves meanwhile gives up on ApplyTimeout as
// unavailable and says that a majority did not confirm its leadership.
func TestQuorumReadThatNoMajorityConfirmsSaysSo(t *testing.T) {
	c := newTunedTestCluster(t, 2, leaseOutlastingApplyTimeout)
	leader := c.leader()
	// The first read establishes the term, so the next one only asks a
	// majority to confirm the leader.
	if _, err := leader.ReadState(t.Context()); err != nil {
		t.Fatal(err)
	}
	term := leader.raft.CurrentTerm()
	if err := pauseAll(newSilentRaftLoopPause(t, otherVoter(t, c, leader))); err != nil {
		t.Fatal(err)
	}
	_, err := leader.ReadState(context.Background())
	want := fmt.Sprintf("a majority did not confirm this node's leadership in term %d within %s", term, leader.config.ApplyTimeout)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), want) {
		t.Fatalf("a quorum read no majority confirmed failed with %v, not as unavailable saying %q", err, want)
	}
	if !leader.Status().IsLeader {
		t.Fatal("leader stepped down; the read was not ended by its bound")
	}
}
