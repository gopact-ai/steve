package nodewire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WriteSize prefixes a blob with its length.
func WriteSize(w io.Writer, size int64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(size))
	_, err := w.Write(buf[:])
	return err
}

// ReadSize reads a blob's length prefix.
func ReadSize(r io.Reader) (int64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, fmt.Errorf("blob size: %w", err)
	}
	size := binary.BigEndian.Uint64(buf[:])
	if size > 1<<40 {
		return 0, fmt.Errorf("blob size %d is absurd", size)
	}
	return int64(size), nil
}
