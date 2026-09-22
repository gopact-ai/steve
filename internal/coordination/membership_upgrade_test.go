package coordination

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/hashicorp/raft"
)

func legacySnapshotMetadata(t *testing.T, state State, receipts map[string]receipt, omit ...string) []byte {
	t.Helper()
	metadata, err := json.Marshal(snapshotData{Format: 3, State: state, Receipts: receipts})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &envelope); err != nil {
		t.Fatal(err)
	}
	var encodedState map[string]json.RawMessage
	if err := json.Unmarshal(envelope["state"], &encodedState); err != nil {
		t.Fatal(err)
	}
	for _, field := range omit {
		delete(encodedState, field)
	}
	envelope["state"], err = json.Marshal(encodedState)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

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
	data := legacySnapshotMetadata(t, m.state, map[string]receipt{}, "pending_joins", "pending_votes")
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

func TestLegacySnapshotRequiresBothFenceMapsToBeMissing(t *testing.T) {
	m := newMachine("upgrade", nil)
	m.state.Members["hub"] = Member{NodeID: "hub", Address: "hub:1"}
	m.state.Replicas["hub"] = "hub:1"
	m.state.Coordinator = Assignment{NodeID: "hub", Epoch: 1}
	m.state.Audit = []AuditRecord{{Kind: "coordinator_initialized", To: "hub"}}
	for _, tc := range []struct {
		name string
		omit string
	}{
		{name: "pending_joins_missing", omit: "pending_joins"},
		{name: "pending_votes_missing", omit: "pending_votes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := legacySnapshotMetadata(t, m.state, map[string]receipt{}, tc.omit)
			sink := &snapshotMemorySink{}
			if err := (&encodedSnapshot{metadata: data}).Persist(sink); err != nil {
				t.Fatal(err)
			}
			if err := newMachine("upgrade", nil).Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, ErrInvalid) {
				t.Fatalf("snapshot with one missing fence map was accepted: %v", err)
			}
		})
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
	for _, tc := range []struct {
		name  string
		joins bool
		votes bool
	}{
		{"null-joins", true, false},
		{"null-votes", false, true},
		{"both-null", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine("upgrade", nil)
			if tc.joins {
				m.state.PendingJoins = nil
			}
			if tc.votes {
				m.state.PendingVotes = nil
			}
			metadata := legacySnapshotMetadata(t, m.state, map[string]receipt{})
			sink := &snapshotMemorySink{}
			if err := (&encodedSnapshot{metadata: metadata}).Persist(sink); err != nil {
				t.Fatal(err)
			}
			if err := newMachine("upgrade", nil).Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, ErrInvalid) {
				t.Fatalf("new snapshot with null membership fences was accepted: %v", err)
			}
		})
	}
}

func TestLegacySnapshotAfterControlActivationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		omit []string
	}{
		{"old-writer", []string{"required_control_protocol", "pending_joins", "pending_votes"}},
		{"floor-only", []string{"required_control_protocol"}},
		{"joins-only", []string{"pending_joins"}},
		{"votes-only", []string{"pending_votes"}},
		{"both-maps", []string{"pending_joins", "pending_votes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine("upgrade", nil)
			m.state.Members["hub"] = Member{NodeID: "hub", Address: "hub:1"}
			m.state.Replicas["hub"] = "hub:1"
			m.state.Coordinator = Assignment{NodeID: "hub", Epoch: 1}
			m.state.RequiredControlProtocol = ControlProtocolVersion
			m.state.Audit = []AuditRecord{{Kind: "coordinator_initialized", To: "hub"}, {Kind: "control_protocol_required", Reason: "protocol=1"}}
			data := legacySnapshotMetadata(t, m.state, map[string]receipt{}, tc.omit...)
			sink := &snapshotMemorySink{}
			if err := (&encodedSnapshot{metadata: data}).Persist(sink); err != nil {
				t.Fatal(err)
			}
			before := m.read()
			if err := m.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, ErrInvalid) {
				t.Fatalf("activated snapshot with lost additive state was accepted: %v", err)
			}
			if !reflect.DeepEqual(before, m.read()) {
				t.Fatal("rejected snapshot changed state")
			}
		})
	}
}

func TestSnapshotFormat3PreservesAdditiveControlState(t *testing.T) {
	m := newMachine("upgrade", nil)
	m.state.PendingJoins["joining"] = true
	m.state.PendingVotes["peer"] = VotingRequest{ID: "grant", NodeID: "peer", Voting: true}
	m.state.RequiredControlProtocol = ControlProtocolVersion
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	metadata, _, err := decodeSnapshot(bytes.NewReader(sink.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var envelope snapshotData
	if err := json.Unmarshal(metadata, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Format != 3 {
		t.Fatalf("rolling upgrade snapshot format = %d; old readers require 3", envelope.Format)
	}
	restored := newMachine("upgrade", nil)
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.read(), restored.read()) {
		t.Fatalf("format3 lost additive control state: %+v", restored.read())
	}
}

func TestSnapshotRejectsUnreleasedFormat4(t *testing.T) {
	m := newMachine("upgrade", nil)
	data := snapshotData{Format: 4, State: m.read(), Receipts: map[string]receipt{}}
	metadata, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	sink := &snapshotMemorySink{}
	if err := (&encodedSnapshot{metadata: metadata}).Persist(sink); err != nil {
		t.Fatal(err)
	}
	before := m.read()
	if err := m.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unreleased snapshot format4 accepted: %v", err)
	}
	if !reflect.DeepEqual(before, m.read()) {
		t.Fatal("rejected snapshot changed state")
	}
}
