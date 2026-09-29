package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// A coordinator that does not lead consensus keeps its business generation
// only while a quorum keeps confirming its replica: it gives the generation
// up when a confirmation fails, or when none has succeeded for three
// ApplyTimeouts, although its replica still names it and hears a leader. A
// coordinator that leads consensus needs no confirmation. Observations
// carry their own times, so nothing here depends on scheduling.
func TestCoordinatorThatDoesNotLeadConsensusKeepsItsGenerationWhileAQuorumConfirmsItsReplica(t *testing.T) {
	const timeout = time.Second
	const self, other = "node-1", "node-2"
	assignment := coordination.Assignment{NodeID: self, Epoch: 3}
	started := time.Now()
	failed := fmt.Errorf("%w: the replica could not be confirmed with a quorum: %w", coordination.ErrUnavailable, errors.New("the read index could not be reached"))
	for _, tc := range []struct {
		name   string
		leader string
		after  time.Duration
		// failed is whether a confirmation that started at ApplyTimeout,
		// after the one the generation started on, failed.
		failed bool
		want   string
	}{
		{name: "leader needs none", leader: self, after: 10 * timeout},
		{name: "leader after a failed confirmation", leader: self, after: timeout + time.Millisecond, failed: true},
		{name: "follower confirmed recently", leader: other, after: timeout / 2},
		{name: "follower whose confirmation failed", leader: other, after: timeout + time.Millisecond, failed: true, want: "could not be confirmed with a quorum"},
		{name: "follower unconfirmed for three ApplyTimeouts", leader: other, after: 3*timeout + time.Millisecond, want: "no quorum has confirmed this replica"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runtime := &Runtime{
				config:  Config{Coordination: coordination.Config{NodeID: self, ApplyTimeout: timeout}},
				current: &generation{Activation: Activation{NodeID: self, Assignment: assignment, WriterGeneration: 7, Context: ctx}, cancel: cancel},
			}
			// The generation started on a quorum read at 0.
			runtime.confirmation.settle(started, nil)
			if tc.failed {
				runtime.confirmation.settle(started.Add(timeout), failed)
			}
			seen := observation{at: started.Add(tc.after), log: coordination.LogProgress{Committed: 10, Applied: 10}}
			seen.Healthy, seen.LeaderID, seen.IsLeader = true, tc.leader, tc.leader == self
			seen.Coordinator, seen.WriterGeneration, seen.AppliedIndex = assignment, 7, 10
			seen.Members = map[string]coordination.Member{self: {NodeID: self, Address: "127.0.0.1:1"}}
			seen.Replicas = map[string]string{self: "127.0.0.1:1"}
			state := tickState{liveness: liveness{started: started, heard: started}}
			err := runtime.step(seen, &state)
			if tc.want == "" && err != nil {
				t.Fatalf("the coordinator gave up its generation %s after it was last confirmed: %v", tc.after, err)
			}
			if tc.want != "" && (!errors.Is(err, coordination.ErrUnavailable) || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("the coordinator reported %v %s after it was last confirmed, want it to give its generation up because %s", err, tc.after, tc.want)
			}
		})
	}
}

// A coordinator that was replaced while its replica received heartbeats
// but no log entries still names itself, so no local view tells it that it
// no longer coordinates. It gives its business generation up nonetheless,
// about two ApplyTimeouts and two polls after it was replaced, while its
// replica still lags.
func TestReplacedCoordinatorGivesUpItsGenerationWhileItsReplicationLags(t *testing.T) {
	hub := startTestHub(t)
	gate := newEntryGate()
	old := joinNonvoter(t, hub, gate.wrap)
	t.Cleanup(gate.release)
	transferCoordinator(t, hub, old, "hand over")
	active := WaitPeerReady(t, old)
	gate.paused.Store(true)
	transferCoordinator(t, hub, hub, strings.Repeat("take back ", 1600))
	replaced := time.Now()
	WaitPeerReady(t, hub)
	runtime := old.Runtime.Load()
	if seen := runtime.Status().Coordinator; seen.NodeID != old.Config.NodeID {
		t.Fatalf("the replaced coordinator's replica applied the transfer to %s; its replication was meant to lag", seen.NodeID)
	}
	timeout := runtime.config.Coordination.ApplyTimeout
	bound := 2*timeout + 3*time.Second
	select {
	case <-active.Context.Done():
	case <-time.After(bound - time.Since(replaced)):
		t.Fatalf("the replaced coordinator kept business generation %d %s after it was replaced, while its replication lagged: %+v", active.Generation, bound, runtime.Status())
	}
	status := runtime.Status()
	if status.Ready {
		t.Fatalf("the replaced coordinator's generation %d ended, but it still reports ready: %+v", active.Generation, status)
	}
	if status.Coordinator.NodeID != old.Config.NodeID {
		t.Fatal("the generation ended once the replica caught up with the transfer, not while it lagged")
	}
	t.Logf("the generation ended %s after the coordinator was replaced: %s", time.Since(replaced).Round(time.Millisecond), status.LastError)
}

// A coordinator that does not lead consensus, and whose replica receives
// heartbeats but no log entries, cannot tell whether it still coordinates.
// It gives its business generation up about two ApplyTimeouts and two
// polls after its replica falls behind, because its replica cannot be
// confirmed with a quorum, and starts another once its replica has caught
// up.
func TestCoordinatorWhoseReplicationLagsGivesUpItsGenerationUntilItCatchesUp(t *testing.T) {
	hub := startTestHub(t)
	gate := newEntryGate()
	member := joinNonvoter(t, hub, gate.wrap)
	t.Cleanup(gate.release)
	transferCoordinator(t, hub, member, "hand over")
	active := WaitPeerReady(t, member)
	leader := hub.Runtime.Load()
	state, err := leader.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	gate.paused.Store(true)
	// The large command ID makes the entry larger than any heartbeat.
	if _, err := leader.Rename(t.Context(), coordination.RenameRequest{ID: "rename-" + strings.Repeat("x", 16000), Actor: "owner", ExpectedRevision: state.Revision, NodeID: member.Config.NodeID, Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	lagged := time.Now()
	runtime := member.Runtime.Load()
	timeout := runtime.config.Coordination.ApplyTimeout
	bound := 2*timeout + 3*time.Second
	status := runtime.Status()
	for status.Ready {
		if time.Since(lagged) > bound {
			t.Fatalf("a coordinator whose replication lagged kept business generation %d for %s: %+v", active.Generation, bound, status)
		}
		time.Sleep(10 * time.Millisecond)
		status = runtime.Status()
	}
	if !strings.Contains(status.LastError, "could not be confirmed with a quorum") {
		t.Fatalf("the coordinator gave its generation up, but not because its replica could not be confirmed: %s", status.LastError)
	}
	if status.Members[member.Config.NodeID].Name == "renamed" {
		t.Fatal("the generation ended once the replica caught up, not while it lagged")
	}
	if active.Context.Err() == nil {
		t.Fatalf("the coordinator reports no ready generation, but generation %d runs on", active.Generation)
	}
	t.Logf("the generation ended %s after the replica fell behind", time.Since(lagged).Round(time.Millisecond))
	gate.release()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.Status().Members[member.Config.NodeID].Name != "renamed" {
		if time.Now().After(deadline) {
			t.Fatal("the replica did not catch up within 10s of its replication resuming")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fresh := WaitPeerReady(t, member); fresh.Generation <= active.Generation {
		t.Fatalf("generation %d was published, not a new one after generation %d", fresh.Generation, active.Generation)
	}
}
