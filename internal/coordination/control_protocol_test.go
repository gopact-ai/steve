package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func addProtocolTestReplica(t *testing.T, c *testCluster, id string) *Service {
	t.Helper()
	cfg := c.configs["node-1"]
	cfg.NodeID, cfg.FailureDomain = id, "domain-"+id
	cfg.DataDir, cfg.BindAddress, cfg.Bootstrap = t.TempDir(), "127.0.0.1:0", false
	peer, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.nodes[id] = peer
	c.mu.Unlock()
	c.configs[id] = cfg
	return peer
}

func assertNoControlLogAfter(t *testing.T, leader *Service, index uint64) {
	t.Helper()
	for next := index + 1; next <= leader.raft.LastIndex(); next++ {
		var entry raft.Log
		if err := leader.store.GetLog(next, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == raft.LogCommand || entry.Type == raft.LogConfiguration {
			t.Fatalf("gate rejection wrote log index %d type %v: %s", next, entry.Type, entry.Data)
		}
	}
}

func TestNewControlsRequireAllReplicaCapabilitiesBeforeLog(t *testing.T) {
	for _, operation := range []string{"voting", "nonvoter-transfer"} {
		for _, failure := range []string{"old-protocol", "unavailable"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				c := newTestCluster(t, 1)
				leader, _ := joinNonvoter(t, c)
				bystander := addProtocolTestReplica(t, c, "bystander")
				if _, err := leader.Join(t.Context(), JoinRequest{ID: "join-bystander", Actor: "owner", Member: Member{NodeID: "bystander", Address: bystander.Status().Address}}); err != nil {
					t.Fatal(err)
				}
				before := leader.Status().State
				if before.RequiredControlProtocol != 0 {
					t.Fatal("ordinary joins activated new controls")
				}
				lastIndex := leader.raft.LastIndex()
				var upgraded atomic.Bool
				leader.config.Probe = func(ctx context.Context, member Member) (Progress, error) {
					progress, err := c.probe(ctx, member)
					if member.NodeID == "bystander" && !upgraded.Load() {
						if failure == "unavailable" {
							return Progress{}, ErrUnavailable
						}
						progress.ControlProtocol = 0
					}
					return progress, err
				}
				vote := VotingRequest{ID: "new-control", Actor: "owner", NodeID: "new-node", Voting: true, ExpectedRevision: before.Revision}
				transfer := TransferRequest{ID: "new-control", Actor: "owner", TargetNodeID: "new-node", ExpectedEpoch: before.Coordinator.Epoch}
				invoke := func() error {
					if operation == "voting" {
						_, err := leader.SetVoting(t.Context(), vote)
						return err
					}
					_, err := leader.Transfer(t.Context(), transfer)
					return err
				}
				if err := invoke(); !errors.Is(err, ErrNotReady) {
					t.Fatalf("new control bypassed old/offline nonvoter bystander: %v", err)
				}
				if !reflect.DeepEqual(before, leader.Status().State) {
					t.Fatal("gate rejection changed state or left preparation")
				}
				assertNoControlLogAfter(t, leader, lastIndex)
				upgraded.Store(true)
				if err := invoke(); err != nil {
					t.Fatalf("same request could not succeed after upgrade: %v", err)
				}
				if leader.Status().RequiredControlProtocol != ControlProtocolVersion {
					t.Fatal("new control did not persist required protocol")
				}
				snapshot, err := leader.fsm.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				defer snapshot.Release()
				sink := &snapshotMemorySink{}
				if err := snapshot.Persist(sink); err != nil {
					t.Fatal(err)
				}
				restored := newMachine(before.ClusterID, nil)
				if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
					t.Fatal(err)
				}
				if restored.read().RequiredControlProtocol != ControlProtocolVersion {
					t.Fatal("snapshot lost the activated protocol floor")
				}
			})
		}
	}
}

func TestActivatedControlsAllowOfflineBystander(t *testing.T) {
	for _, operation := range []string{"revoke-offline-vote", "grant-healthy-vote", "nonvoter-transfer"} {
		t.Run(operation, func(t *testing.T) {
			c := newTestCluster(t, 3)
			leader, _ := joinNonvoter(t, c)
			// A reviewed no-op vote change still commits the new preparation
			// semantics and their protocol floor while all replicas are online.
			if _, err := leader.SetVoting(t.Context(), VotingRequest{ID: "activate", Actor: "owner", NodeID: "new-node", ExpectedRevision: leader.Status().Revision}); err != nil {
				t.Fatal(err)
			}
			if leader.Status().RequiredControlProtocol != ControlProtocolVersion {
				t.Fatal("protocol floor was not activated")
			}
			offline := ""
			for id := range leader.Status().Voters {
				if id != leader.config.NodeID && id != leader.Status().Coordinator.NodeID {
					offline = id
					break
				}
			}
			if offline == "" {
				t.Fatal("no bystander voter to stop")
			}
			// Stop the actual Raft replica, not only its capability probe.
			// The two surviving voters still have quorum.
			c.stop(offline)
			state := leader.Status().State
			if operation == "nonvoter-transfer" {
				if _, err := leader.Transfer(t.Context(), TransferRequest{ID: "after-outage", Actor: "owner", TargetNodeID: "new-node", ExpectedEpoch: state.Coordinator.Epoch}); err != nil {
					t.Fatalf("offline bystander blocked healthy Hub transfer: %v", err)
				}
				if leader.Status().Coordinator.NodeID != "new-node" {
					t.Fatal("transfer did not settle")
				}
				return
			}
			target, voting := "new-node", true
			if operation == "revoke-offline-vote" {
				target, voting = offline, false
			}
			if _, err := leader.SetVoting(t.Context(), VotingRequest{ID: "after-outage", Actor: "owner", NodeID: target, Voting: voting, ExpectedRevision: state.Revision}); err != nil {
				t.Fatalf("offline replica blocked %s: %v", operation, err)
			}
			got := leader.Status()
			if (got.Voters[target] != "") != voting || got.Members[target].Voting != voting || len(got.PendingVotes) != 0 {
				t.Fatalf("vote change did not settle: %+v", got.State)
			}
		})
	}
}

func TestActivatedControlsStillRequireTargetProgress(t *testing.T) {
	for _, operation := range []string{"voting", "nonvoter-transfer"} {
		t.Run(operation, func(t *testing.T) {
			c := newTestCluster(t, 1)
			leader, _ := joinNonvoter(t, c)
			if _, err := leader.SetVoting(t.Context(), VotingRequest{ID: "activate", Actor: "owner", NodeID: "new-node", ExpectedRevision: leader.Status().Revision}); err != nil {
				t.Fatal(err)
			}
			c.stop("new-node")
			state := leader.Status().State
			var err error
			if operation == "voting" {
				_, err = leader.SetVoting(t.Context(), VotingRequest{ID: "offline-target", Actor: "owner", NodeID: "new-node", Voting: true, ExpectedRevision: state.Revision})
			} else {
				_, err = leader.Transfer(t.Context(), TransferRequest{ID: "offline-target", Actor: "owner", TargetNodeID: "new-node", ExpectedEpoch: state.Coordinator.Epoch})
			}
			if !errors.Is(err, ErrNotReady) {
				t.Fatalf("activated protocol bypassed target progress: %v", err)
			}
			got := leader.Status()
			if got.Coordinator != state.Coordinator || got.Voters["new-node"] != "" {
				t.Fatal("unavailable target gained coordinator authority or a vote")
			}
		})
	}
}

func TestControlProtocolCapabilitySurvivesRPC(t *testing.T) {
	c := newTLSTestCluster(t, 2)
	progress, err := c.clients["node-1"].Probe(t.Context(), c.members["node-2"])
	if err != nil {
		t.Fatal(err)
	}
	if progress.ControlProtocol != ControlProtocolVersion {
		t.Fatalf("authenticated Status/Progress RPC lost capability: %+v", progress)
	}
}

func TestControlActivationLeavesLegacySnapshotMarker(t *testing.T) {
	for _, kind := range []string{"voting_prepare", "transfer"} {
		t.Run(kind, func(t *testing.T) {
			m := newMachine("upgrade", nil)
			for _, id := range []string{"hub", "peer"} {
				m.state.Members[id] = Member{NodeID: id, Address: id + ":1"}
				m.state.Replicas[id] = id + ":1"
			}
			m.state.Voters["hub"] = "hub:1"
			m.state.Coordinator = Assignment{NodeID: "hub", Epoch: 1}
			cmd := command{
				Kind: kind, ID: "activate", Actor: "owner", ClusterID: "upgrade",
				Time:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Voting:   VotingRequest{ID: "activate", Actor: "owner", NodeID: "peer", Voting: true},
				Transfer: TransferRequest{ID: "activate", Actor: "owner", TargetNodeID: "peer", ExpectedEpoch: 1},
			}
			apply := func(index uint64, c command) receipt {
				t.Helper()
				raw, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				return m.Apply(&raft.Log{Index: index, Data: raw}).(receipt)
			}
			for _, index := range []uint64{2, 3} {
				if result := apply(index, cmd); result.err() != nil {
					t.Fatal(result.err())
				}
			}
			// Another accepted operation and then a rejected stale one must
			// not create a second activation marker.
			next := command{Kind: "voting_prepare", ID: "next", Actor: "owner", ClusterID: "upgrade",
				Voting: VotingRequest{ID: "next", Actor: "owner", NodeID: "hub", Voting: true, ExpectedRevision: m.read().Revision}}
			if result := apply(4, next); result.err() != nil {
				t.Fatal(result.err())
			}
			next.ID = "stale"
			next.Voting.ExpectedRevision = 0
			if result := apply(5, next); result.err() == nil {
				t.Fatal("stale voting preparation unexpectedly succeeded")
			}
			count := 0
			for _, event := range m.read().Audit {
				if event.Kind == "control_protocol_required" {
					count++
					if event.Index != 2 || event.CommandID != "activate/control-protocol" || event.Actor != cmd.Actor || !event.Time.Equal(cmd.Time) {
						t.Fatalf("marker lost committed command identity: %+v", event)
					}
				}
			}
			if count != 1 {
				t.Fatalf("control activation did not leave exactly one legacy-compatible audit marker: %d", count)
			}
		})
	}
}

func TestJoinOldProtocolAllowedBeforeActivationAndRejectedAfter(t *testing.T) {
	c := newTestCluster(t, 1)
	leader := c.leader()
	first := addProtocolTestReplica(t, c, "first")
	var upgraded atomic.Bool
	leader.config.Probe = func(ctx context.Context, member Member) (Progress, error) {
		progress, err := c.probe(ctx, member)
		if !upgraded.Load() || member.NodeID == "late" {
			progress.ControlProtocol = 0
		}
		return progress, err
	}
	if _, err := leader.Join(t.Context(), JoinRequest{ID: "legacy-join", Actor: "owner", Member: Member{NodeID: "first", Address: first.Status().Address}}); err != nil {
		t.Fatalf("ordinary join interrupted rolling upgrade: %v", err)
	}
	upgraded.Store(true)
	if _, err := leader.Transfer(t.Context(), TransferRequest{ID: "activate", Actor: "owner", TargetNodeID: "first", ExpectedEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	late := addProtocolTestReplica(t, c, "late")
	before := leader.Status().State
	index := leader.raft.LastIndex()
	request := JoinRequest{ID: "legacy-after-activation", Actor: "owner", Member: Member{NodeID: "late", Address: late.Status().Address}}
	if _, err := leader.Join(t.Context(), request); !errors.Is(err, ErrNotReady) {
		t.Fatalf("old peer admitted after activation: %v", err)
	}
	if !reflect.DeepEqual(before, leader.Status().State) || len(leader.TransportPeers()) != 2 {
		t.Fatal("rejected Join changed membership")
	}
	assertNoControlLogAfter(t, leader, index)
	leader.config.Probe = c.probe
	if _, err := leader.Join(t.Context(), request); err != nil {
		t.Fatalf("upgraded Join retry failed: %v", err)
	}
}

func TestJoinFSMRechecksActivatedProtocol(t *testing.T) {
	m := newMachine("upgrade", nil)
	m.state.RequiredControlProtocol = ControlProtocolVersion
	c := command{Member: Member{NodeID: "old", Address: "old:1", StorageLevel: "restricted"}}
	r := receipt{}
	applyJoinPrepare(&m.state, c, &r)
	if r.err() == nil || len(m.state.Members) != 0 || len(m.state.PendingJoins) != 0 {
		t.Fatalf("FSM admitted old protocol: %+v", r)
	}
	c.MemberControlProtocol = ControlProtocolVersion
	r = receipt{}
	applyJoinPrepare(&m.state, c, &r)
	if r.err() != nil || !m.state.PendingJoins["old"] {
		t.Fatalf("FSM rejected supported protocol: %+v", r)
	}
}

func TestConcurrentJoinCannotBypassControlActivation(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	late := addProtocolTestReplica(t, c, "late")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	leader.config.Probe = func(ctx context.Context, member Member) (Progress, error) {
		progress, err := c.probe(ctx, member)
		if member.NodeID == "late" {
			progress.ControlProtocol = 0
		}
		if member.NodeID == "new-node" {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return progress, err
	}
	transferred := make(chan error, 1)
	go func() {
		_, err := leader.Transfer(t.Context(), TransferRequest{ID: "activate", Actor: "owner", TargetNodeID: "new-node", ExpectedEpoch: 1})
		transferred <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("capability gate did not run")
	}
	joined := make(chan error, 1)
	go func() {
		_, err := leader.Join(t.Context(), JoinRequest{ID: "racing-old-join", Actor: "owner", Member: Member{NodeID: "late", Address: late.Status().Address}})
		joined <- err
	}()
	unblock()
	if err := <-transferred; err != nil {
		t.Fatal(err)
	}
	if err := <-joined; !errors.Is(err, ErrNotReady) {
		t.Fatalf("concurrent Join bypassed activation: %v", err)
	}
	if _, ok := leader.Status().Members["late"]; ok {
		t.Fatal("old candidate left an admission")
	}
}
