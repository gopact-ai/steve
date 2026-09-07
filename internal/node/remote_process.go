package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/node/journal"
	"github.com/gopact-ai/steve/internal/nodewire"
)

const inputBufferBytes = 1 << 20

var errInputBufferFull = errors.New("node: disconnected input buffer exceeds 1 MiB")

type inputLine struct {
	seq  uint64
	data []byte
}

// remoteProcess outlives individual wire streams. ACP keeps its client and
// session map across a disconnect; it sees EOF only on terminal failure.
type remoteProcess struct {
	stopped               atomic.Bool
	transport             remoteTransport
	id                    string
	grace                 time.Duration
	stdout                *io.PipeReader
	output                *io.PipeWriter
	mu                    sync.Mutex
	writeMu               sync.Mutex
	conn                  *conn
	stream                *nodewire.Stream
	ready, resumable      bool
	in, out, sent, haveIn uint64
	next                  uint64
	queue                 []inputLine
	bytes                 int
	partial               []byte
	// JSON-RPC responses have no method. Matching them to agent requests
	// lets this layer remember permission/elicitation answers without
	// changing acphost's permission broker or prompting the user twice.
	requests  map[string]bool
	answered  map[string]bool
	changed   chan struct{}
	done      chan struct{}
	err       error
	closeOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
}

func newRemoteProcess(t remoteTransport, c *conn, stream *nodewire.Stream) *remoteProcess {
	r, w := io.Pipe()
	grace := SessionGrace
	if ms := c.getAdvert().SessionGraceMS; ms > 0 {
		grace = time.Duration(ms) * time.Millisecond
	}
	p := &remoteProcess{transport: t, id: stream.Request().Stream, grace: grace, stdout: r, output: w,
		conn: c, stream: stream, ready: true, resumable: true, changed: make(chan struct{}), done: make(chan struct{}),
		requests: map[string]bool{}, answered: map[string]bool{}}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	go p.writeLoop()
	go p.readLoop(c, stream, bufio.NewReader(stream))
	return p
}

func (p *remoteProcess) Stdout() io.ReadCloser { return remoteStdout{p} }
func (p *remoteProcess) Stdin() io.WriteCloser { return p }

type remoteStdout struct{ p *remoteProcess }

func (s remoteStdout) Read(b []byte) (int, error) { return s.p.stdout.Read(b) }
func (s remoteStdout) Close() error               { return s.p.Close() }
func (p *remoteProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == io.EOF {
		return nil
	}
	return p.err
}
func (p *remoteProcess) Kill()         { _ = p.Close() }
func (p *remoteProcess) Stopped() bool { return p.stopped.Load() }

func (p *remoteProcess) wakeLocked() { close(p.changed); p.changed = make(chan struct{}) }

func (p *remoteProcess) finish(err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: session grace exceeded", io.EOF)
	}
	if err == nil || err.Error() == "exit 0" {
		err = io.EOF
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return
	}
	p.err, p.ready = err, false
	p.queue, p.partial = nil, nil
	p.bytes = 0
	stream := p.stream
	close(p.done)
	p.cancel()
	p.wakeLocked()
	p.mu.Unlock()
	_ = p.output.CloseWithError(err)
	go func() { _ = stream.Close() }()
}

func (p *remoteProcess) Close() error {
	p.closeOnce.Do(func() {
		p.finish(io.EOF)
		_ = p.stdout.Close()
		// Close during an outage still means release. On the next connection
		// send the process id explicitly, without resuming it or its sessions.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), p.grace)
			defer cancel()
			c, err := p.transport.registry.awaitConnection(ctx, p.transport.node)
			if err != nil {
				return
			}
			stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamRelease, Stream: p.id})
			if err == nil {
				if awaitExit(ctx, stream, p.transport.node) == nil {
					p.stopped.Store(true)
				}
				_ = stream.Close()
			}
		}()
	})
	return nil
}

func (p *remoteProcess) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return 0, err
	}
	if (!p.ready || !p.conn.alive()) && p.bytes+len(p.partial)+len(b) > inputBufferBytes {
		p.mu.Unlock()
		return 0, errInputBufferFull
	}
	for p.bytes > 0 && p.bytes+len(p.partial)+len(b) > inputBufferBytes && p.ready && p.err == nil {
		changed, lost := p.changed, p.conn.mux.Done()
		p.mu.Unlock()
		select {
		case <-changed:
		case <-lost:
		}
		p.mu.Lock()
		if !p.conn.alive() {
			p.ready = false
		}
	}
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return 0, err
	}
	if !p.ready && p.bytes+len(p.partial)+len(b) > inputBufferBytes {
		p.mu.Unlock()
		return 0, errInputBufferFull
	}
	data := append(p.partial, b...)
	p.partial = nil
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if len(data) <= journal.MaxLine {
				p.partial = bytes.Clone(data)
				break
			}
			// Keep an oversized active write moving, but never replay a
			// fragment that could already have reached the agent's stdin.
			p.resumable = false
			i = len(data) - 1
		}
		line := bytes.Clone(data[:i+1])
		if len(line) > journal.MaxLine {
			p.resumable = false
		}
		if line[len(line)-1] == '\n' {
			p.in++
		}
		p.next++
		p.rememberAnswer(line)
		p.queue = append(p.queue, inputLine{p.next, line})
		p.bytes += len(line)
		data = data[i+1:]
	}
	target := p.next
	p.wakeLocked()
	// Active writes retain ordinary pipe backpressure. During disconnection
	// they return as soon as the bounded buffer has accepted the input.
	for p.ready && p.sent < target && p.err == nil {
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-p.done:
		}
		p.mu.Lock()
	}
	err := p.err
	if err == nil && !p.ready && p.bytes+len(p.partial) > inputBufferBytes {
		err = errInputBufferFull
	}
	p.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *remoteProcess) acknowledgeLocked(seq uint64) error {
	if seq > p.in || seq < p.haveIn {
		return errors.New("node: inconsistent input acknowledgement")
	}
	p.haveIn = seq
	p.discardInputLocked(seq)
	return nil
}

func (p *remoteProcess) discardInputLocked(seq uint64) {
	n := 0
	for n < len(p.queue) && p.queue[n].seq <= seq {
		p.bytes -= len(p.queue[n].data)
		p.queue[n].data = nil
		n++
	}
	p.queue = p.queue[n:]
	p.wakeLocked()
}

func (p *remoteProcess) writeLoop() {
	for {
		p.mu.Lock()
		if p.err != nil {
			p.mu.Unlock()
			return
		}
		stream, changed := p.stream, p.changed
		ack, acked := stream.InputAck()
		if p.resumable && ack > p.haveIn {
			if err := p.acknowledgeLocked(ack); err != nil {
				p.mu.Unlock()
				p.finish(err)
				return
			}
		}
		var next *inputLine
		if p.ready {
			for _, line := range p.queue {
				if line.seq > p.sent {
					copy := line
					next = &copy
					break
				}
			}
		}
		p.mu.Unlock()
		if next == nil {
			select {
			case <-changed:
			case <-acked:
			case <-p.done:
				return
			}
			continue
		}
		_, err := stream.Write(next.data)
		p.mu.Lock()
		if p.stream == stream {
			if err == nil {
				p.sent = next.seq
			} else {
				p.ready = false
			}
			if !p.resumable && err == nil {
				p.discardInputLocked(p.sent)
			}
			p.wakeLocked()
		}
		p.mu.Unlock()
		if err != nil && !errors.Is(err, nodewire.ErrMuxClosed) {
			select {
			case <-stream.Done():
			default:
				p.finish(err)
				return
			}
		}
	}
}

func (p *remoteProcess) readLoop(c *conn, stream *nodewire.Stream, reader *bufio.Reader) {
	for {
		err := readLines(reader, func(line []byte, complete bool) error {
			p.mu.Lock()
			if !complete || len(line) > journal.MaxLine {
				p.resumable = false
			}
			if complete {
				p.out++
			}
			skip := complete && p.duplicateRequest(line)
			p.mu.Unlock()
			if skip {
				return nil
			}
			_, err := p.output.Write(line)
			return err
		})
		p.mu.Lock()
		canResume := p.resumable && p.err == nil && !c.released.Load() && errors.Is(err, nodewire.ErrMuxClosed)
		p.ready = false
		p.wakeLocked()
		p.mu.Unlock()
		if !canResume {
			if err != nil && strings.HasPrefix(err.Error(), nodewire.ExitPrefix) {
				p.stopped.Store(true)
			}
			p.finish(err)
			return
		}
		p.transport.registry.down(c)
		ctx, cancel := context.WithTimeout(p.ctx, p.grace)
		start := time.Now()
		// Bound open and ResumeAck too, including a peer that accepts a
		// socket but never answers. Only a successful ResumeAck ends this wait.
		watch := time.AfterFunc(p.grace, func() { p.finish(fmt.Errorf("%w: session grace exceeded", io.EOF)) })
		for {
			c, err = p.transport.registry.awaitConnection(ctx, p.transport.node)
			if err != nil {
				break
			}
			if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureJournal) {
				err = journal.ErrUnresumable
				break
			}
			p.mu.Lock()
			if p.err != nil {
				err = p.err
				p.mu.Unlock()
				break
			}
			req := nodewire.OpenRequest{Kind: nodewire.StreamACP, Harness: p.transport.harness, Stream: p.id, Resume: true, AfterOut: p.out, AfterIn: p.in}
			p.mu.Unlock()
			stream, err = c.mux.Open(req)
			if err != nil {
				p.transport.registry.down(c)
				continue
			}
			p.mu.Lock()
			if p.err != nil {
				err = p.err
				p.mu.Unlock()
				_ = stream.Close()
				break
			}
			p.conn, p.stream = c, stream
			p.mu.Unlock()
			reader = bufio.NewReader(stream)
			var ack nodewire.ResumeAck
			ack, err = nodewire.ReadResumeAck(reader)
			if err != nil {
				if errors.Is(err, nodewire.ErrMuxClosed) {
					p.transport.registry.down(c)
					continue
				}
				break
			}
			p.mu.Lock()
			err = p.acknowledgeLocked(ack.HaveIn)
			if p.err != nil {
				err = p.err
			}
			if err == nil && p.err == nil {
				p.sent, p.ready = ack.HaveIn, true
				p.wakeLocked()
			}
			p.mu.Unlock()
			if err == nil {
				log.Printf("node: %s reattached stream %s after %s (after output %d, accepted input %d)", p.transport.node, p.id, time.Since(start).Round(time.Millisecond), req.AfterOut, ack.HaveIn)
			}
			break
		}
		watch.Stop()
		cancel()
		if err != nil {
			p.finish(err)
			return
		}
	}
}

type rpcLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (p *remoteProcess) duplicateRequest(line []byte) bool {
	var rpc rpcLine
	if json.Unmarshal(line, &rpc) != nil || len(rpc.ID) == 0 {
		return false
	}
	if rpc.Method != "session/request_permission" && rpc.Method != "elicitation/create" {
		return false
	}
	id := string(rpc.ID)
	p.requests[id] = true
	return p.answered[id]
}

func (p *remoteProcess) rememberAnswer(line []byte) {
	var rpc rpcLine
	if json.Unmarshal(line, &rpc) != nil || rpc.Method != "" || len(rpc.ID) == 0 {
		return
	}
	id := string(rpc.ID)
	if p.requests[id] {
		p.answered[id] = true
	}
}
