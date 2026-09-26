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
func snapshotBoundaryFollower(t *testing.T, trailing uint64) (*NetworkTransport, *NetworkTransport, *InmemStore, *MockFSM, RPCHeader, *Raft) {
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
	return leader, follower, store, fsm, header, node
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
			leader, follower, store, fsm, header, _ := snapshotBoundaryFollower(t, trailing)
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
	leader, follower, store, _, header, _ := snapshotBoundaryFollower(t, 0)
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
		{"before-snapshot", 3, 99, 2, true},
		{"before-snapshot-unchecked-term", 3, 99, 3, true},
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

// The leader resends from its last acknowledged index, which can be below the
// follower's snapshot: the follower installed a later snapshot than the batch
// start, or took its own snapshot after storing a batch whose response was
// lost.
func TestRaft_AppendEntriesSnapshotBoundaryBelowSnapshot(t *testing.T) {
	t.Run("installed-snapshot", func(t *testing.T) {
		leader, follower, store, fsm, header, _ := snapshotBoundaryFollower(t, 0)
		request := snapshotBoundaryBatch(header)
		request.PrevLogEntry, request.PrevLogTerm = 90, 2
		var below []*Log
		for i := uint64(91); i <= 100; i++ {
			below = append(below, &Log{Index: i, Term: 2, Type: LogCommand, Data: []byte(fmt.Sprintf("entry-%d", i))})
		}
		request.Entries = append(below, request.Entries...)
		var response AppendEntriesResponse
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
		require.True(t, response.Success, "a batch that starts below the snapshot must succeed")

		var compacted Log
		require.ErrorIs(t, store.GetLog(100, &compacted), ErrLogNotFound, "entries in the snapshot must not be stored")
		last, err := store.LastIndex()
		require.NoError(t, err)
		require.Equal(t, uint64(164), last)
		require.Eventually(t, func() bool { return len(fsm.Logs()) == 64 }, 2*time.Second, time.Millisecond)
		for i, data := range fsm.Logs() {
			require.Equal(t, []byte(fmt.Sprintf("entry-%d", i+101)), data, "entries in the snapshot must not be applied")
		}
	})

	for _, trailing := range []uint64{0, DefaultConfig().TrailingLogs} {
		t.Run(fmt.Sprintf("own-snapshot-trailing-%d", trailing), func(t *testing.T) {
			leader, follower, store, fsm, header, node := snapshotBoundaryFollower(t, trailing)
			request := snapshotBoundaryBatch(header)
			var response AppendEntriesResponse
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.True(t, response.Success)
			require.Eventually(t, func() bool { return len(fsm.Logs()) == 64 }, 2*time.Second, time.Millisecond)
			require.NoError(t, node.Snapshot().Error())
			if trailing == 0 {
				var compacted Log
				require.ErrorIs(t, store.GetLog(164, &compacted), ErrLogNotFound)
			}

			// Treat the ACK as lost: resend the stored batch after the
			// follower's snapshot has covered it, with the next entry.
			request.LeaderCommitIndex = 165
			request.Entries = append(request.Entries, &Log{Index: 165, Term: 3, Type: LogCommand, Data: []byte("entry-165")})
			response = AppendEntriesResponse{}
			require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
			require.True(t, response.Success, "a retry anchored below the follower's snapshot must succeed")

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

// staleTailFollower is a follower that holds 1..110 from term 1, of which
// only 1..10 were committed: 11..110 came from a deposed leader. The term 2
// leader holds 1..10 from term 1 and 11..164 from term 2, has committed all
// of them and has compacted them into a snapshot at 100.
func staleTailFollower(t *testing.T, trailing uint64) (*NetworkTransport, *NetworkTransport, *Raft, *MockFSM, RPCHeader) {
	t.Helper()
	follower, err := NewTCPTransport("127.0.0.1:0", nil, 2, 2*time.Second, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { follower.Close() })
	leader, err := NewTCPTransport("127.0.0.1:0", nil, 2, 2*time.Second, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { leader.Close() })

	store, fsm := NewInmemStore(), &MockFSM{}
	var logs []*Log
	for i := uint64(1); i <= 110; i++ {
		logs = append(logs, &Log{Index: i, Term: 1, Type: LogCommand, Data: []byte(fmt.Sprintf("stale-%d", i))})
	}
	require.NoError(t, store.StoreLogs(logs))
	require.NoError(t, store.SetUint64(keyCurrentTerm, 1))

	config := DefaultConfig()
	config.LocalID = "follower"
	config.HeartbeatTimeout = time.Hour
	config.ElectionTimeout = time.Hour
	config.SnapshotInterval = time.Hour
	config.TrailingLogs = trailing
	config.LogOutput = io.Discard
	node, err := NewRaft(config, fsm, store, store, NewInmemSnapshotStore(), follower)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, node.Shutdown().Error()) })

	header := RPCHeader{ProtocolVersion: ProtocolVersionMax, ID: []byte("leader"), Addr: []byte(leader.LocalAddr())}
	return leader, follower, node, fsm, header
}

// staleTailInstall installs the leader's snapshot at 100. Compaction keeps the
// follower's stale 101..110, which no request has checked yet.
func staleTailInstall(t *testing.T, leader, follower *NetworkTransport, header RPCHeader) {
	t.Helper()
	members := Configuration{Servers: []Server{
		{ID: "leader", Address: leader.LocalAddr(), Suffrage: Voter},
		{ID: "follower", Address: follower.LocalAddr(), Suffrage: Nonvoter},
	}}
	data := []byte{0x90}
	request := &InstallSnapshotRequest{
		RPCHeader: header, SnapshotVersion: SnapshotVersionMax, Term: 2,
		LastLogIndex: 100, LastLogTerm: 2,
		Configuration: EncodeConfiguration(members), ConfigurationIndex: 1,
		Size: int64(len(data)),
	}
	var response InstallSnapshotResponse
	require.NoError(t, leader.InstallSnapshot("follower", follower.LocalAddr(), request, &response, bytes.NewReader(data)))
	require.True(t, response.Success)
}

// staleTailAppend sends the leader's entries prev+1..last and reports whether
// the follower accepted them.
func staleTailAppend(t *testing.T, leader, follower *NetworkTransport, header RPCHeader, prev, last uint64) bool {
	t.Helper()
	prevTerm := uint64(2)
	if prev <= 10 {
		prevTerm = 1
	}
	request := &AppendEntriesRequest{RPCHeader: header, Term: 2, PrevLogEntry: prev, PrevLogTerm: prevTerm, LeaderCommitIndex: 164}
	for i := prev + 1; i <= last; i++ {
		request.Entries = append(request.Entries, &Log{Index: i, Term: 2, Type: LogCommand, Data: []byte(fmt.Sprintf("leader-%d", i))})
	}
	var response AppendEntriesResponse
	require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
	return response.Success
}

// A request proves the follower's log matches the leader's only up to the
// last entry it covers, so the follower commits no further than that, however
// far the leader has committed and however long the follower's log is.
func TestRaft_AppendEntriesSnapshotBoundaryCommitIndex(t *testing.T) {
	for _, trailing := range []uint64{0, DefaultConfig().TrailingLogs} {
		t.Run(fmt.Sprintf("below-snapshot-batch-trailing-%d", trailing), func(t *testing.T) {
			leader, follower, node, fsm, header := staleTailFollower(t, trailing)
			staleTailInstall(t, leader, follower, header)

			require.True(t, staleTailAppend(t, leader, follower, header, 90, 100), "a batch below the snapshot must succeed")
			require.LessOrEqual(t, node.getCommitIndex(), uint64(100), "a batch ending at 100 must not commit the unchecked 101..110")
			require.Equal(t, uint64(100), node.getLastApplied())
			require.Empty(t, fsm.Logs())
		})

		// The leader backtracks over the stale tail and falls back to its
		// snapshot. One backtracking request, delayed on an abandoned
		// connection, arrives after the snapshot is installed.
		t.Run(fmt.Sprintf("delayed-backtrack-trailing-%d", trailing), func(t *testing.T) {
			leader, follower, node, fsm, header := staleTailFollower(t, trailing)
			require.False(t, staleTailAppend(t, leader, follower, header, 110, 164))
			require.False(t, staleTailAppend(t, leader, follower, header, 30, 94))
			require.False(t, staleTailAppend(t, leader, follower, header, 20, 84))
			staleTailInstall(t, leader, follower, header)

			require.True(t, staleTailAppend(t, leader, follower, header, 30, 94), "the delayed request lies below the snapshot")
			require.LessOrEqual(t, node.getCommitIndex(), uint64(100), "the delayed request must not commit the unchecked 101..110")

			require.True(t, staleTailAppend(t, leader, follower, header, 100, 164))
			var want [][]byte
			for i := 101; i <= 164; i++ {
				want = append(want, []byte(fmt.Sprintf("leader-%d", i)))
			}
			require.Eventually(t, func() bool { return len(fsm.Logs()) >= len(want) }, 2*time.Second, time.Millisecond)
			require.Equal(t, want, fsm.Logs(), "the follower must apply the leader's entries, never its stale tail")
		})

		// A request that covers less than the leader has committed, such as
		// one delayed behind a later batch, never moves the commit index back.
		t.Run(fmt.Sprintf("delayed-request-keeps-commit-trailing-%d", trailing), func(t *testing.T) {
			leader, follower, node, _, header := staleTailFollower(t, trailing)
			staleTailInstall(t, leader, follower, header)
			require.True(t, staleTailAppend(t, leader, follower, header, 100, 130))
			require.Equal(t, uint64(130), node.getCommitIndex())

			require.True(t, staleTailAppend(t, leader, follower, header, 30, 94), "the delayed request lies below the snapshot")
			require.Equal(t, uint64(130), node.getCommitIndex(), "the commit index must never decrease")
		})
	}

	// A caught-up follower learns the commit index from a request without
	// entries, anchored at its last entry.
	t.Run("caught-up-empty-request", func(t *testing.T) {
		leader, follower, _, fsm, header, node := snapshotBoundaryFollower(t, 0)
		request := snapshotBoundaryBatch(header)
		request.LeaderCommitIndex = 150
		var response AppendEntriesResponse
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
		require.True(t, response.Success)
		require.Equal(t, uint64(150), node.getCommitIndex())

		empty := &AppendEntriesRequest{RPCHeader: header, Term: 3, PrevLogEntry: 164, PrevLogTerm: 3, LeaderCommitIndex: 164}
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), empty, &response))
		require.True(t, response.Success)
		require.Equal(t, uint64(164), node.getCommitIndex(), "a request without entries must advance the commit index up to its predecessor")
		require.Eventually(t, func() bool { return len(fsm.Logs()) == 64 }, 2*time.Second, time.Millisecond)
	})

	// A heartbeat carries neither a predecessor nor a commit index and leaves
	// the commit index alone.
	t.Run("heartbeat-keeps-commit", func(t *testing.T) {
		leader, follower, _, _, header, node := snapshotBoundaryFollower(t, 0)
		request := snapshotBoundaryBatch(header)
		request.LeaderCommitIndex = 150
		var response AppendEntriesResponse
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), request, &response))
		require.True(t, response.Success)

		heartbeat := &AppendEntriesRequest{RPCHeader: header, Term: 3}
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), heartbeat, &response))
		require.True(t, response.Success)
		require.Equal(t, uint64(150), node.getCommitIndex())
	})
}
