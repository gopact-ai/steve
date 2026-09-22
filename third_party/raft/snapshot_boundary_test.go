// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the real transport and follower with a compacted snapshot boundary.
// The snapshot term differs from the suffix term so confusing either with the
// current term cannot accidentally pass predecessor validation.
func snapshotBoundaryFollower(t *testing.T, trailing uint64) (*NetworkTransport, *NetworkTransport, *InmemStore, *MockFSM, RPCHeader) {
	t.Helper()
	follower, err := NewTCPTransport("127.0.0.1:0", nil, 2, 2*time.Second, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { follower.Close() })
	leader, err := NewTCPTransport("127.0.0.1:0", nil, 2, 2*time.Second, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { leader.Close() })

	config := DefaultConfig()
	config.LocalID = "follower"
	config.HeartbeatTimeout = time.Hour
	config.ElectionTimeout = time.Hour
	config.SnapshotInterval = time.Hour
	config.TrailingLogs = trailing
	config.LogOutput = io.Discard
	store, fsm := NewInmemStore(), &MockFSM{}
	node, err := NewRaft(config, fsm, store, store, NewInmemSnapshotStore(), follower)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, node.Shutdown().Error()) })

	header := RPCHeader{ProtocolVersion: ProtocolVersionMax, ID: []byte("leader"), Addr: []byte(leader.LocalAddr())}
	members := Configuration{Servers: []Server{
		{ID: "leader", Address: leader.LocalAddr(), Suffrage: Voter},
		{ID: "follower", Address: follower.LocalAddr(), Suffrage: Nonvoter},
	}}
	// An empty MessagePack array is an empty MockFSM snapshot. All data is synthetic.
	data := []byte{0x90}
	request := &InstallSnapshotRequest{
		RPCHeader: header, SnapshotVersion: SnapshotVersionMax, Term: 3,
		LastLogIndex: 100, LastLogTerm: 2,
		Configuration: EncodeConfiguration(members), ConfigurationIndex: 1,
		Size: int64(len(data)),
	}
	var response InstallSnapshotResponse
	require.NoError(t, leader.InstallSnapshot("follower", follower.LocalAddr(), request, &response, bytes.NewReader(data)))
	require.True(t, response.Success)
	var compacted Log
	require.ErrorIs(t, store.GetLog(100, &compacted), ErrLogNotFound)
	return leader, follower, store, fsm, header
}

func snapshotBoundaryBatch(header RPCHeader) *AppendEntriesRequest {
	request := &AppendEntriesRequest{
		RPCHeader: header, Term: 3, PrevLogEntry: 100, PrevLogTerm: 2,
		LeaderCommitIndex: 164,
	}
	for i := uint64(101); i <= 164; i++ {
		request.Entries = append(request.Entries, &Log{
			Index: i, Term: 3, Type: LogCommand, Data: []byte(fmt.Sprintf("entry-%d", i)),
		})
	}
	return request
}

func TestRaft_AppendEntriesSnapshotBoundaryLostACK(t *testing.T) {
	for _, trailing := range []uint64{0, DefaultConfig().TrailingLogs} {
		t.Run(fmt.Sprintf("trailing-%d", trailing), func(t *testing.T) {
			leader, follower, store, fsm, header := snapshotBoundaryFollower(t, trailing)
			request := snapshotBoundaryBatch(header)
			var response AppendEntriesResponse
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.True(t, response.Success)

			// Treat the successful ACK as lost: do not advance the sender's
			// predecessor, and retransmit exactly the same already-stored batch.
			response = AppendEntriesResponse{}
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.True(t, response.Success, "a retry anchored at the installed snapshot must succeed")
			require.Equal(t, uint64(164), response.LastLog)

			request = &AppendEntriesRequest{
				RPCHeader: header, Term: 3, PrevLogEntry: 164, PrevLogTerm: 3,
				LeaderCommitIndex: 165,
				Entries:           []*Log{{Index: 165, Term: 3, Type: LogCommand, Data: []byte("entry-165")}},
			}
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.True(t, response.Success, "normal replication must continue after the retry")
			var next Log
			require.NoError(t, store.GetLog(165, &next))
			require.Equal(t, []byte("entry-165"), next.Data)
			require.Eventually(t, func() bool { return len(fsm.Logs()) == 65 }, 2*time.Second, time.Millisecond)
			for i, data := range fsm.Logs() {
				require.Equal(t, []byte(fmt.Sprintf("entry-%d", i+101)), data, "retry must not reapply commands")
			}
		})
	}
}

func TestRaft_AppendEntriesSnapshotBoundaryValidation(t *testing.T) {
	leader, follower, store, _, header := snapshotBoundaryFollower(t, 0)
	var response AppendEntriesResponse
	require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), snapshotBoundaryBatch(header), &response))
	require.True(t, response.Success)

	for _, tc := range []struct {
		name                     string
		term, previous, prevTerm uint64
		success                  bool
	}{
		{"snapshot-correct-term", 3, 100, 2, true},
		{"snapshot-wrong-term", 3, 100, 3, false},
		{"older-request-term", 2, 100, 2, false},
		{"stored-log-correct-term", 3, 120, 3, true},
		{"stored-log-wrong-term", 3, 120, 2, false},
		{"tip-correct-term", 3, 164, 3, true},
		{"tip-wrong-term", 3, 164, 2, false},
		{"missing-before-snapshot", 3, 99, 2, false},
		{"missing-after-tip", 3, 165, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &AppendEntriesRequest{
				RPCHeader: header, Term: tc.term,
				PrevLogEntry: tc.previous, PrevLogTerm: tc.prevTerm,
				LeaderCommitIndex: 164,
			}
			var response AppendEntriesResponse
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.Equal(t, tc.success, response.Success)
			require.Equal(t, uint64(3), response.Term)
			last, err := store.LastIndex()
			require.NoError(t, err)
			require.Equal(t, uint64(164), last, "validation must not change the stored suffix")
		})
	}
}
