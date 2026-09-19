package coordination

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/hashicorp/raft"
)

const snapshotMagic = "STVFSM02"
const snapshotHeaderSize = 24 + sha256.Size

type encodedSnapshot struct{ metadata, application []byte }

func (s *encodedSnapshot) Persist(sink raft.SnapshotSink) error {
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
	return nil
}

func (s *encodedSnapshot) Release() {
	s.metadata = nil
	s.application = nil
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
