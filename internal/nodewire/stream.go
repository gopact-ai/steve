package nodewire

import (
	"errors"
	"io"
	"sync"
)

// Stream is one multiplexed conversation — an ACP session's stdio, or the
// reverse MCP channel. It is an io.ReadWriteCloser so acphost can hand it
// straight to acp.NewClient without knowing a network is involved.
type Stream struct {
	mux *Mux
	id  uint32
	req OpenRequest

	// frames is never closed: the receive loop and the shutdown path both
	// touch this stream, and a closed queue would make one of them panic.
	// done is the single end-of-stream signal instead.
	frames chan []byte
	done   chan struct{}

	finishOnce sync.Once
	closeOnce  sync.Once

	mu      sync.Mutex
	pending []byte
	readErr error
}

func newStream(m *Mux, id uint32, req OpenRequest) *Stream {
	return &Stream{
		mux: m, id: id, req: req,
		frames: make(chan []byte, streamBuffer),
		done:   make(chan struct{}),
	}
}

// Request is what the peer said this stream was for.
func (s *Stream) Request() OpenRequest { return s.req }

// Done closes when the stream ends, however it ended — the peer closed it,
// or the connection dropped. A remote agent's Wait blocks on this.
func (s *Stream) Done() <-chan struct{} { return s.done }

func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	select {
	case chunk := <-s.frames:
		return s.consume(p, chunk), nil
	case <-s.done:
		// Bytes that arrived before the close are still owed to the reader;
		// only report the error once the queue is drained.
		select {
		case chunk := <-s.frames:
			return s.consume(p, chunk), nil
		default:
			return 0, s.err()
		}
	}
}

func (s *Stream) consume(p, chunk []byte) int {
	n := copy(p, chunk)
	if n < len(chunk) {
		s.mu.Lock()
		s.pending = chunk[n:]
		s.mu.Unlock()
	}
	return n
}

// Write splits oversized writes so a caller never has to know the frame cap.
func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > MaxPayload {
			chunk = chunk[:MaxPayload]
		}
		// The payload must outlive this call — callers reuse their buffers.
		buf := make([]byte, len(chunk))
		copy(buf, chunk)
		if err := s.mux.write(Frame{Stream: s.id, Kind: KindData, Payload: buf}); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// Close ends the stream for both sides. It is safe to call more than once,
// which matters because acphost closes stdin and stdout separately and both
// map onto this one stream.
func (s *Stream) Close() error { return s.CloseWithReason("") }

// CloseWithReason ends the stream and tells the peer why. An exec stream
// uses it to carry the exit status; the peer's Read returns the reason as
// its error once the buffered output is drained.
func (s *Stream) CloseWithReason(reason string) error {
	var err error
	s.closeOnce.Do(func() {
		err = s.mux.write(Frame{Stream: s.id, Kind: KindClose, Payload: []byte(reason)})
		s.mux.drop(s.id)
		s.finish(ErrStreamClosed)
	})
	if errors.Is(err, ErrMuxClosed) {
		// The connection went away first; the stream is closed either way.
		return nil
	}
	return err
}

func (s *Stream) deliver(payload []byte) {
	if len(payload) == 0 {
		return
	}
	select {
	case s.frames <- payload:
	case <-s.done:
	case <-s.mux.done:
	}
}

func (s *Stream) remoteClosed(reason string) {
	err := io.EOF
	if reason != "" && reason != ErrMuxClosed.Error() {
		err = errors.New(reason)
	}
	s.finish(err)
}

func (s *Stream) finish(err error) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		if s.readErr == nil {
			s.readErr = err
		}
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *Stream) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr == nil {
		return io.EOF
	}
	return s.readErr
}
