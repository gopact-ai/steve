// Package nodewire carries authenticated node operations over multiplexed
// streams. Node-owned sessions keep ACP clients and their reverse callbacks on
// the execution node; older peers can still transport raw ACP process streams.
package nodewire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kind tags what a frame does to its stream.
type Kind uint8

const (
	KindOpen     Kind = 1 // start a stream; payload is a JSON OpenRequest
	KindData     Kind = 2 // stream bytes
	KindClose    Kind = 3 // stream ended; payload is an optional reason
	KindPing     Kind = 4 // liveness; stream id is unused
	KindInputAck Kind = 5 // journal.v1: accepted input cursor, big-endian uint64
	KindGoodbye  Kind = 6 // journal.v1: explicit hub release; stream id is unused
)

// MaxPayload bounds one frame so a peer cannot make the other side allocate
// without limit. ACP messages are JSON-RPC lines, far below this.
const MaxPayload = 1 << 20

// headerSize is stream id (4) + kind (1) + length (4).
const headerSize = 9

var ErrPayloadTooLarge = errors.New("nodewire: payload exceeds maximum")

type Frame struct {
	Stream  uint32
	Kind    Kind
	Payload []byte
}

// WriteFrame emits one frame. Callers must serialise writes on a connection;
// Mux owns that lock.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return ErrPayloadTooLarge
	}
	var header [headerSize]byte
	binary.BigEndian.PutUint32(header[0:4], f.Stream)
	header[4] = byte(f.Kind)
	binary.BigEndian.PutUint32(header[5:9], uint32(len(f.Payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(f.Payload) == 0 {
		return nil
	}
	_, err := w.Write(f.Payload)
	return err
}

// ReadFrame reads one frame. The returned payload is freshly allocated, so
// the caller may hold it past the next read.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(header[5:9])
	if length > MaxPayload {
		return Frame{}, fmt.Errorf("%w: %d", ErrPayloadTooLarge, length)
	}
	f := Frame{
		Stream: binary.BigEndian.Uint32(header[0:4]),
		Kind:   Kind(header[4]),
	}
	if length == 0 {
		return f, nil
	}
	f.Payload = make([]byte, length)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, err
	}
	return f, nil
}
