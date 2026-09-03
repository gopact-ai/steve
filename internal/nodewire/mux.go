package nodewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// OpenRequest says what a new stream is for. Both ends can open streams: the
// hub opens ACP sessions and verification commands, and the node opens the
// reverse channel that lets a remote agent reach the hub's loopback MCP
// server.
type OpenRequest struct {
	Kind    string `json:"kind"`
	Harness string `json:"harness,omitempty"`
	// Command and Dir describe a StreamExec: a shell command the node runs
	// in a directory, whose output flows back on the stream and whose exit
	// status is the close reason.
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

const (
	// StreamACP carries one agent's stdio for the life of a session.
	StreamACP = "acp"
	// StreamMCP is the reverse channel to the hub's agentmcp server. It runs
	// on this same connection so the agent still only ever talks to a
	// loopback address on its own machine.
	StreamMCP = "mcp"
	// StreamExec runs one command on the node and returns its output. It
	// exists for verification: a step's check has to run where the step's
	// work is, and taking the agent's word for it is not verification.
	StreamExec = "exec"
	// StreamAdvert asks the node to check itself again and send a fresh
	// advert, so a harness repaired after the handshake shows up without
	// dropping the connection and every session on it.
	StreamAdvert = "advert"
	// StreamBlob moves one file between hub and node, size-prefixed: the
	// hub's "put <name>" sends 8 bytes of big-endian length then the bytes;
	// "get <name>" receives the same. Artifacts travel this way as git
	// bundles; git on each side does the rest.
	StreamBlob = "blob"
	// StreamGrant tells a node to admit one peer, once, for one blob: the
	// command is "<token> <name> <seconds>". StreamFetch tells a node to
	// fetch a blob from a peer that granted it: "<addr> <token> <name>".
	// Together they move an artifact between two nodes without the hub in
	// the data path; the hub stays the one that decides.
	StreamGrant = "grant"
	StreamFetch = "fetch"
	// StreamAdmit asks the node to re-check a requirement on a fresh
	// observation of itself before an attempt runs there: the hub's
	// placement was made on a snapshot that may since have changed. The
	// request and reply are one JSON document each.
	StreamAdmit = "admit"
	// StreamSkills asks the node to materialize the skill bundle it was
	// just sent as a blob: the command is "apply <bundle hash>". The node
	// answers with an exit status like StreamExec.
	StreamSkills = "skills"
	// StreamRelease tells the node an attempt is over: the bindings it
	// minted for it are dropped and the servers behind them stopped. The
	// command is the attempt id.
	StreamRelease = "release"
	// StreamConfig reads or rewrites what a node offers — its AI tools,
	// the commands it checks for, its MCP servers, its declarations — so
	// a machine is configured from the hub's page and keeps the result in
	// its own node.json. The command is "get" or "set"; a set carries a
	// Settings document and answers with the settings as applied.
	StreamConfig = "config"
)

// ExitPrefix leads the close reason of a finished exec stream, followed by
// the exit code — "exit 0" is success, anything else names the failure.
const ExitPrefix = "exit "

var (
	ErrMuxClosed    = errors.New("nodewire: connection closed")
	ErrStreamClosed = errors.New("nodewire: stream closed")
)

// streamBuffer is how many frames may queue for a stream nobody is reading.
// Past it the receive loop blocks, which stalls the whole connection — the
// deliberate tradeoff of one connection per node. ACP traffic is small
// request/response JSON, so the queue exists for scheduling jitter, not for
// buffering a firehose.
const streamBuffer = 64

// Mux multiplexes independent streams over one connection. Stream ids are
// partitioned by role — the dialer uses odd ids, the listener even — so both
// ends can open without negotiating.
type Mux struct {
	conn io.ReadWriteCloser

	writeMu sync.Mutex

	mu       sync.Mutex
	streams  map[uint32]*Stream
	nextID   uint32
	closed   bool
	closeErr error

	incoming chan *Stream
	done     chan struct{}
}

// NewMux starts multiplexing. dialer selects the id parity; the side that
// dialed the connection passes true.
func NewMux(conn io.ReadWriteCloser, dialer bool) *Mux {
	first := uint32(2)
	if dialer {
		first = 1
	}
	m := &Mux{
		conn:     conn,
		streams:  map[uint32]*Stream{},
		nextID:   first,
		incoming: make(chan *Stream, streamBuffer),
		done:     make(chan struct{}),
	}
	go m.receive()
	return m
}

// Open starts a stream and tells the peer what it is for.
func (m *Mux) Open(req OpenRequest) (*Stream, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode open: %w", err)
	}
	m.mu.Lock()
	if m.closed {
		err := m.closeErr
		m.mu.Unlock()
		if err == nil {
			err = ErrMuxClosed
		}
		return nil, err
	}
	id := m.nextID
	m.nextID += 2
	s := newStream(m, id, req)
	m.streams[id] = s
	m.mu.Unlock()

	if err := m.write(Frame{Stream: id, Kind: KindOpen, Payload: payload}); err != nil {
		m.drop(id)
		return nil, err
	}
	return s, nil
}

// Accept returns the next stream the peer opened.
func (m *Mux) Accept(ctx context.Context) (*Stream, error) {
	select {
	case s, ok := <-m.incoming:
		if !ok {
			return nil, m.err()
		}
		return s, nil
	case <-m.done:
		return nil, m.err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Ping checks the connection without disturbing any stream.
func (m *Mux) Ping() error { return m.write(Frame{Kind: KindPing}) }

func (m *Mux) Close() error {
	m.shutdown(ErrMuxClosed)
	return m.conn.Close()
}

// Done closes when the connection drops, so a node registry can notice a
// node going away without polling.
func (m *Mux) Done() <-chan struct{} { return m.done }

func (m *Mux) err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closeErr != nil {
		return m.closeErr
	}
	return ErrMuxClosed
}

func (m *Mux) write(f Frame) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return WriteFrame(m.conn, f)
}

func (m *Mux) receive() {
	for {
		f, err := ReadFrame(m.conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrMuxClosed
			}
			m.shutdown(err)
			return
		}
		switch f.Kind {
		case KindPing:
			continue
		case KindOpen:
			m.accept(f)
		case KindData:
			if s := m.lookup(f.Stream); s != nil {
				s.deliver(f.Payload)
			}
		case KindClose:
			if s := m.lookup(f.Stream); s != nil {
				s.remoteClosed(string(f.Payload))
			}
			m.drop(f.Stream)
		}
	}
}

func (m *Mux) accept(f Frame) {
	var req OpenRequest
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		_ = m.write(Frame{Stream: f.Stream, Kind: KindClose, Payload: []byte("bad open request")})
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	s := newStream(m, f.Stream, req)
	m.streams[f.Stream] = s
	m.mu.Unlock()
	select {
	case m.incoming <- s:
	case <-m.done:
	}
}

func (m *Mux) lookup(id uint32) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

func (m *Mux) drop(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

func (m *Mux) shutdown(cause error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.closeErr = cause
	streams := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		streams = append(streams, s)
	}
	m.streams = map[uint32]*Stream{}
	close(m.done)
	m.mu.Unlock()
	for _, s := range streams {
		s.remoteClosed(cause.Error())
	}
}
