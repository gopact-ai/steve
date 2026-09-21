package coordination

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/hashicorp/raft"
)

func TestOldSnapshotDoesNotAdmitIncompleteJoin(t *testing.T) {
	m := newMachine("upgrade", nil)
	for _, id := range []string{"hub", "joined", "incomplete", "rejoining"} {
		m.state.Members[id] = Member{NodeID: id, Address: id + ":1"}
		m.state.Replicas[id] = id + ":1"
	}
	m.state.Coordinator = Assignment{NodeID: "hub", Epoch: 1}
	m.state.Audit = []AuditRecord{{Kind: "coordinator_initialized", To: "hub"}, {Kind: "member_joined", To: "joined"}, {Kind: "member_joined", To: "rejoining"}, {Kind: "member_removed", From: "rejoining"}}
	m.state.PendingJoins = nil
	m.state.PendingVotes = nil
	data, err := json.Marshal(snapshotData{Format: 3, State: m.state, Receipts: map[string]receipt{}})
	if err != nil {
		t.Fatal(err)
	}
	sink := &snapshotMemorySink{}
	if err := (&encodedSnapshot{metadata: data}).Persist(sink); err != nil {
		t.Fatal(err)
	}
	restored := newMachine("upgrade", nil)
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hub", "joined"} {
		if !restored.state.IsActiveReplica(id) {
			t.Fatalf("completed old member %s was lost", id)
		}
	}
	for _, id := range []string{"incomplete", "rejoining"} {
		if restored.state.IsActiveReplica(id) {
			t.Fatalf("old incomplete member %s acquired authority", id)
		}
	}
}

func TestHistoricalVoteLogReplaysWithoutPreparation(t *testing.T) {
	m := newMachine("upgrade", nil)
	m.state.Members["peer"] = Member{NodeID: "peer", Address: "peer:1"}
	m.state.Replicas["peer"] = "peer:1"
	m.state.Voters["peer"] = "peer:1"
	c := command{Kind: "voting", ID: "old-vote", Actor: "owner", ClusterID: "upgrade", Voting: VotingRequest{ID: "old-vote", Actor: "owner", NodeID: "peer", Voting: true}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	result := m.Apply(&raft.Log{Index: 2, Data: raw}).(receipt)
	if result.err() != nil || !m.state.Members["peer"].Voting {
		t.Fatalf("historical vote could not replay: %+v", result)
	}
}

func TestCurrentSnapshotRequiresPendingMembershipState(t *testing.T) {
	m := newMachine("upgrade", nil)
	m.state.PendingJoins = nil
	metadata, err := json.Marshal(snapshotData{Format: snapshotFormat, State: m.state, Receipts: map[string]receipt{}})
	if err != nil {
		t.Fatal(err)
	}
	sink := &snapshotMemorySink{}
	if err := (&encodedSnapshot{metadata: metadata}).Persist(sink); err != nil {
		t.Fatal(err)
	}
	if err := newMachine("upgrade", nil).Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err == nil {
		t.Fatal("new snapshot without membership fences was accepted")
	}
}
