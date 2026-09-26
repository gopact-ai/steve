// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

// The slow link carries a 512 KiB batch in about 500ms, well past the fixed
// 200ms transport timeout. With one timeout per 64 KiB the transport assumes
// at least 320 KiB/s, a third of what the link delivers. A buffered link
// holds a whole batch, so a write can return before any of it arrives.
const (
	slowLinkRate    = 1 << 20
	slowLinkTimeout = 200 * time.Millisecond
	slowLinkScale   = 64 << 10
	slowLinkEntries = 16
	slowLinkEntry   = 32 << 10
	slowLinkChunk   = 4 << 10
	slowLinkBuffer  = slowLinkEntries * slowLinkEntry
)

// slowLink paces the bytes written on every connection one side dials, like
// streams multiplexed on one slow session. A write returns once no more than
// buffer bytes are still ahead of its end; with no buffer it returns once its
// bytes have crossed. A write that cannot get that far before the
// connection's write deadline fails at the deadline, and its remaining bytes
// are never delivered.
type slowLink struct {
	buffer int

	mu   sync.Mutex
	next time.Time
}

func slowLinkDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / slowLinkRate
}

// reserve schedules n more bytes on the link and waits until the write may
// return. It reports when the bytes will have crossed and whether the write
// returned before deadline.
func (l *slowLink) reserve(n int, deadline time.Time) (time.Time, bool) {
	l.mu.Lock()
	now := time.Now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	end := start.Add(slowLinkDuration(n))
	accepted := end.Add(-slowLinkDuration(l.buffer))
	if !deadline.IsZero() && accepted.After(deadline) {
		l.mu.Unlock()
		time.Sleep(time.Until(deadline))
		return time.Time{}, false
	}
	l.next = end
	l.mu.Unlock()
	time.Sleep(time.Until(accepted))
	return end, true
}

type slowLinkConn struct {
	net.Conn
	link *slowLink

	mu       sync.Mutex
	deadline time.Time

	// A buffered link delivers accepted bytes in order from one goroutine.
	queue     chan slowLinkChunkAt
	closed    chan struct{}
	closeOnce sync.Once
}

type slowLinkChunkAt struct {
	data []byte
	at   time.Time
}

func newSlowLinkConn(conn net.Conn, link *slowLink) *slowLinkConn {
	c := &slowLinkConn{Conn: conn, link: link}
	if link.buffer > 0 {
		c.queue = make(chan slowLinkChunkAt, link.buffer/slowLinkChunk+1)
		c.closed = make(chan struct{})
		go c.deliver()
	}
	return c
}

func (c *slowLinkConn) deliver() {
	for {
		select {
		case chunk := <-c.queue:
			select {
			case <-time.After(time.Until(chunk.at)):
			case <-c.closed:
				return
			}
			if _, err := c.Conn.Write(chunk.data); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *slowLinkConn) Close() error {
	if c.closed != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
	return c.Conn.Close()
}

func (c *slowLinkConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *slowLinkConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *slowLinkConn) setWriteDeadline(t time.Time) {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
}

func (c *slowLinkConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := len(p)
		if chunk > slowLinkChunk {
			chunk = slowLinkChunk
		}
		c.mu.Lock()
		deadline := c.deadline
		c.mu.Unlock()
		at, ok := c.link.reserve(chunk, deadline)
		if !ok {
			return written, &net.OpError{Op: "write", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded}
		}
		if c.queue == nil {
			n, err := c.Conn.Write(p[:chunk])
			written += n
			if err != nil {
				return written, err
			}
		} else {
			select {
			case c.queue <- slowLinkChunkAt{data: bytes.Clone(p[:chunk]), at: at}:
				written += chunk
			case <-c.closed:
				return written, net.ErrClosed
			}
		}
		p = p[chunk:]
	}
	return written, nil
}

// slowLinkStream is a StreamLayer whose dialed connections write through
// link; a nil link is a fast one.
type slowLinkStream struct {
	net.Listener
	link *slowLink
}

func (s slowLinkStream) Dial(address ServerAddress, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", string(address), timeout)
	if err != nil || s.link == nil {
		return c, err
	}
	return newSlowLinkConn(c, s.link), nil
}

func slowLinkTransport(t *testing.T, link *slowLink) *NetworkTransport {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	trans := NewNetworkTransportWithConfig(&NetworkTransportConfig{
		Stream: slowLinkStream{Listener: listener, link: link}, MaxPool: 2,
		Timeout: slowLinkTimeout, Logger: hclog.NewNullLogger(),
	})
	trans.TimeoutScale = slowLinkScale
	t.Cleanup(func() { trans.Close() })
	return trans
}

// slowLinkFollower answers every AppendEntries it receives until the test ends.
func slowLinkFollower(t *testing.T) *NetworkTransport {
	t.Helper()
	follower := slowLinkTransport(t, nil)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case rpc := <-follower.Consumer():
				request := rpc.Command.(*AppendEntriesRequest)
				rpc.Respond(&AppendEntriesResponse{Term: request.Term, LastLog: request.Entries[len(request.Entries)-1].Index, Success: true}, nil)
			case <-done:
				return
			}
		}
	}()
	return follower
}

func slowLinkBatch() *AppendEntriesRequest {
	request := &AppendEntriesRequest{RPCHeader: RPCHeader{ProtocolVersion: ProtocolVersionMax, ID: []byte("leader")}, Term: 1, LeaderCommitIndex: slowLinkEntries}
	for i := uint64(1); i <= slowLinkEntries; i++ {
		request.Entries = append(request.Entries, &Log{Index: i, Term: 1, Type: LogCommand, Data: bytes.Repeat([]byte{'x'}, slowLinkEntry)})
	}
	return request
}

func TestNetworkTransport_AppendEntriesScaledTimeout(t *testing.T) {
	t.Run("rpc", func(t *testing.T) {
		follower := slowLinkFollower(t)
		leader := slowLinkTransport(t, &slowLink{})
		var response AppendEntriesResponse
		require.NoError(t, leader.AppendEntries("follower", follower.LocalAddr(), slowLinkBatch(), &response), "a batch the link carries within its size-scaled deadline must succeed")
		require.True(t, response.Success)
	})

	sendPipelined := func(t *testing.T, link *slowLink) {
		follower := slowLinkFollower(t)
		leader := slowLinkTransport(t, link)
		pipeline, err := leader.AppendEntriesPipeline("follower", follower.LocalAddr())
		require.NoError(t, err)
		t.Cleanup(func() { pipeline.Close() })
		// With the default two RPCs in flight the second send waits until the
		// first response is consumed, so send from another goroutine.
		sent := make(chan error, 2)
		go func() {
			for i := 0; i < 2; i++ {
				_, err := pipeline.AppendEntries(slowLinkBatch(), new(AppendEntriesResponse))
				sent <- err
			}
		}()
		for received := 0; received < 2; {
			select {
			case err := <-sent:
				require.NoError(t, err, "a pipelined batch the link carries within its size-scaled deadline must be sent")
			case future := <-pipeline.Consumer():
				require.NoError(t, future.Error(), "a pipelined batch the link carries within its size-scaled deadline must succeed")
				require.True(t, future.Response().Success)
				received++
			case <-time.After(10 * time.Second):
				t.Fatal("no pipelined response")
			}
		}
	}
	// The write returns only after the batch has crossed, so this guards the
	// write deadline.
	t.Run("pipeline", func(t *testing.T) { sendPipelined(t, &slowLink{}) })
	// The write returns while the batch is still buffered, so the response
	// cannot arrive before the rest crosses: this guards the read deadline.
	t.Run("pipeline-buffered", func(t *testing.T) { sendPipelined(t, &slowLink{buffer: slowLinkBuffer}) })

	t.Run("small-request-keeps-fixed-timeout", func(t *testing.T) {
		// Nothing answers, so the request can only end at its deadline.
		follower := slowLinkTransport(t, nil)
		leader := slowLinkTransport(t, &slowLink{})
		request := &AppendEntriesRequest{RPCHeader: RPCHeader{ProtocolVersion: ProtocolVersionMax, ID: []byte("leader")}, Term: 1}
		start := time.Now()
		err := leader.AppendEntries("follower", follower.LocalAddr(), request, new(AppendEntriesResponse))
		require.Error(t, err)
		require.GreaterOrEqual(t, time.Since(start), slowLinkTimeout)
		require.Less(t, time.Since(start), 4*slowLinkTimeout, "a small request must still fail after the fixed timeout")
	})
}

// The deadline grows by one timeout per TimeoutScale bytes of entry data. A
// transport without a positive TimeoutScale keeps the fixed timeout instead
// of dividing by it.
func TestNetworkTransport_AppendEntriesScaledTimeoutValue(t *testing.T) {
	const timeout = time.Second
	entries := func(sizes ...int) *AppendEntriesRequest {
		request := &AppendEntriesRequest{}
		for _, size := range sizes {
			request.Entries = append(request.Entries, &Log{Data: make([]byte, size)})
		}
		return request
	}
	extensions := &AppendEntriesRequest{Entries: []*Log{{Data: make([]byte, 600), Extensions: make([]byte, 424)}}}
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		scale   int
		request *AppendEntriesRequest
		want    time.Duration
	}{
		{"heartbeat", timeout, 1024, entries(), timeout},
		{"empty-entry", timeout, 1024, entries(0), timeout},
		{"half-scale", timeout, 1024, entries(512), timeout + timeout/2},
		{"scale", timeout, 1024, entries(1024), 2 * timeout},
		{"scale-plus-one", timeout, 1024, entries(1024, 1), 2*timeout + timeout/1024},
		{"extensions-count", timeout, 1024, extensions, 2 * timeout},
		{"no-timeout", 0, 1024, entries(4096), 0},
		{"zero-scale", timeout, 0, entries(4096), timeout},
		{"zero-scale-heartbeat", timeout, 0, entries(), timeout},
		{"negative-scale", timeout, -1024, entries(4096), timeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trans := &NetworkTransport{timeout: tc.timeout, TimeoutScale: tc.scale}
			var got time.Duration
			require.NotPanics(t, func() { got = trans.appendEntriesTimeout(tc.request) })
			require.Equal(t, tc.want, got)
		})
	}
}

func slowLinkRaft(t *testing.T, id ServerID, trans *NetworkTransport) *Raft {
	t.Helper()
	config := DefaultConfig()
	config.LocalID = id
	config.HeartbeatTimeout = 100 * time.Millisecond
	config.ElectionTimeout = 100 * time.Millisecond
	config.LeaderLeaseTimeout = 50 * time.Millisecond
	config.LogOutput = io.Discard
	store := NewInmemStore()
	node, err := NewRaft(config, &MockFSM{}, store, store, NewInmemSnapshotStore(), trans)
	require.NoError(t, err)
	t.Cleanup(func() { node.Shutdown().Error() })
	return node
}

// A follower that is behind by more than the link carries in one fixed
// timeout is resent the same batch after every failure, so it catches up only
// if the deadline grows with the batch.
func TestRaft_AppendEntriesScaledTimeoutCatchUp(t *testing.T) {
	leaderTrans := slowLinkTransport(t, &slowLink{})
	followerTrans := slowLinkTransport(t, nil)
	leader := slowLinkRaft(t, "leader", leaderTrans)
	follower := slowLinkRaft(t, "follower", followerTrans)
	require.NoError(t, leader.BootstrapCluster(Configuration{Servers: []Server{{ID: "leader", Address: leaderTrans.LocalAddr(), Suffrage: Voter}}}).Error())
	require.Eventually(t, func() bool { return leader.State() == Leader }, 5*time.Second, 10*time.Millisecond)

	payload := bytes.Repeat([]byte{'x'}, slowLinkEntry)
	var applied ApplyFuture
	for i := 0; i < slowLinkEntries; i++ {
		applied = leader.Apply(payload, time.Second)
	}
	require.NoError(t, applied.Error())
	require.NoError(t, leader.AddNonvoter("follower", followerTrans.LocalAddr(), 0, time.Second).Error())

	target := leader.LastIndex()
	require.Eventually(t, func() bool { return follower.LastIndex() >= target }, 5*time.Second, 20*time.Millisecond,
		"the follower must catch up over the slow link")
}
