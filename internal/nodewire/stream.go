package nodewire

import (
	"bytes"
	"encoding/binary"
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

	// Buffer bytes rather than frames: replaying many short ACP lines must
	// not overflow merely because the reader lost a scheduling timeslice.
	buffer    bytes.Buffer
	available chan struct{}
	done      chan struct{}

	finishOnce sync.Once
	closeOnce  sync.Once

	mu      sync.Mutex
	readErr error
	haveIn  uint64
	acked   chan struct{}
}

func newStream(m *Mux, id uint32, req OpenRequest) *Stream {
	return &Stream{
		mux: m, id: id, req: req,
		available: make(chan struct{}, 1),
		done:      make(chan struct{}),
		acked:     make(chan struct{}, 1),
	}
}

// Request is what the peer said this stream was for.
func (s *Stream) Request() OpenRequest { return s.req }

// Done closes when the stream ends, however it ended — the peer closed it,
// or the connection dropped. A remote agent's Wait blocks on this.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err reports the close cause, including ErrMuxClosed for connection loss.
func (s *Stream) Err() error { return s.err() }

const streamBufferBytes = 16 << 20

func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.mu.Lock()
		if s.buffer.Len() > 0 {
			n, _ := s.buffer.Read(p)
			s.mu.Unlock()
			return n, nil
		}
		err := s.readErr
		s.mu.Unlock()
		if err != nil {
			return 0, err
		}
		select {
		case <-s.available:
		case <-s.done:
		}
	}
}

// Write splits oversized writes so a caller never has to know the frame cap.
func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		select {
		case <-s.done:
			return written, s.err()
		default:
		}
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
		s.finish(ErrStreamClosed)
		err = s.mux.write(Frame{Stream: s.id, Kind: KindClose, Payload: []byte(reason)})
		s.mux.drop(s.id)
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
	s.mu.Lock()
	if s.readErr != nil {
		s.mu.Unlock()
		return
	}
	if s.buffer.Len()+len(payload) <= streamBufferBytes {
		// Writing to a bytes.Buffer cannot fail.
		_, _ = s.buffer.Write(payload)
		s.mu.Unlock()
		select {
		case s.available <- struct{}{}:
		default:
		}
		return
	}
	s.mu.Unlock()
	// A stalled stream must not prevent another stream from receiving
	// cancellation or the connection from detecting a dropped socket.
	s.remoteClosed("slow consumer")
	s.mux.drop(s.id)
	go func() { _ = s.CloseWithReason("slow consumer") }()
}

// AckInput is a sideband cursor: it never enters the ACP byte stream.
func (s *Stream) AckInput(seq uint64) error {
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], seq)
	return s.mux.write(Frame{Stream: s.id, Kind: KindInputAck, Payload: payload[:]})
}

func (s *Stream) inputAck(seq uint64) {
	s.mu.Lock()
	if seq > s.haveIn {
		s.haveIn = seq
	}
	s.mu.Unlock()
	select {
	case s.acked <- struct{}{}:
	default:
	}
}

func (s *Stream) InputAck() (uint64, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.haveIn, s.acked
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
