package coordination

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/hashicorp/raft"
)

const snapshotMagic = "STVFSM02"
const snapshotHeaderSize = 24 + sha256.Size

// encodedSnapshot is a machine snapshot. Its metadata is encoded when the
// snapshot is taken; a checkpoint's application bytes are encoded on the
// first Persist, which runs concurrently with Apply.
type encodedSnapshot struct {
	metadata, application []byte
	checkpoint            Checkpoint
	encoded               bool
	encodeErr             error
	persisted             func() error
}

func (s *encodedSnapshot) encode() error {
	if s.checkpoint != nil && !s.encoded {
		s.encoded = true
		s.application, s.encodeErr = s.checkpoint.Encode()
		if s.encodeErr != nil {
			s.encodeErr = fmt.Errorf("snapshot application: %w", s.encodeErr)
		}
	}
	return s.encodeErr
}

func (s *encodedSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := s.encode(); err != nil {
		_ = sink.Cancel()
		return err
	}
	header := make([]byte, snapshotHeaderSize)
	copy(header, snapshotMagic)
	binary.BigEndian.PutUint64(header[8:16], uint64(len(s.metadata)))
	binary.BigEndian.PutUint64(header[16:24], uint64(len(s.application)))
	hash := sha256.New()
	_, _ = hash.Write(header[:24])
	_, _ = hash.Write(s.metadata)
	_, _ = hash.Write(s.application)
	copy(header[24:], hash.Sum(nil))
	for _, part := range [][]byte{header, s.metadata, s.application} {
		if _, err := io.Copy(sink, bytes.NewReader(part)); err != nil {
			_ = sink.Cancel()
			return err
		}
	}
	if err := sink.Close(); err != nil {
		_ = sink.Cancel()
		return err
	}
	// The snapshot is durable already. A cleanup failure leaves excess live
	// replay evidence, and must not cancel or invalidate that snapshot.
	if s.persisted != nil {
		if err := s.persisted(); err != nil {
			slog.Warn("snapshot persisted; replay cleanup deferred", "error", err)
		}
	}
	return nil
}

func (s *encodedSnapshot) Release() {
	if s.checkpoint != nil {
		s.checkpoint.Release()
		s.checkpoint = nil
	}
	s.metadata = nil
	s.application = nil
	s.persisted = nil
}

func decodeSnapshot(reader io.Reader) ([]byte, []byte, error) {
	header := make([]byte, snapshotHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, nil, fmt.Errorf("read snapshot header: %w", err)
	}
	if string(header[:8]) != snapshotMagic {
		return nil, nil, errors.New("coordination: invalid snapshot format")
	}
	metadataSize, applicationSize := binary.BigEndian.Uint64(header[8:16]), binary.BigEndian.Uint64(header[16:24])
	maxSize := uint64(int(^uint(0) >> 1))
	if metadataSize == 0 || metadataSize > maxSize || applicationSize > maxSize-metadataSize {
		return nil, nil, errors.New("coordination: invalid snapshot lengths")
	}
	// Do not allocate based only on an untrusted header. Limited reads grow
	// with bytes actually received and reject truncation before Restore.
	metadata, err := io.ReadAll(io.LimitReader(reader, int64(metadataSize)))
	if err != nil || uint64(len(metadata)) != metadataSize {
		return nil, nil, fmt.Errorf("read snapshot metadata: %w", errors.Join(err, io.ErrUnexpectedEOF))
	}
	application, err := io.ReadAll(io.LimitReader(reader, int64(applicationSize)))
	if err != nil || uint64(len(application)) != applicationSize {
		return nil, nil, fmt.Errorf("read snapshot application: %w", errors.Join(err, io.ErrUnexpectedEOF))
	}
	var trailing [1]byte
	if _, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		return nil, nil, errors.New("coordination: snapshot has trailing bytes or a read failure")
	}
	hash := sha256.New()
	_, _ = hash.Write(header[:24])
	_, _ = hash.Write(metadata)
	_, _ = hash.Write(application)
	if !bytes.Equal(header[24:], hash.Sum(nil)) {
		return nil, nil, errors.New("coordination: snapshot checksum differs")
	}
	return metadata, application, nil
}
