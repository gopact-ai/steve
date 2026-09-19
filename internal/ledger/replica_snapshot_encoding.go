package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Fixed framing keeps the database binary instead of base64-encoding an entire
// SQLite image. The digest covers the identity, length and database bytes.
const replicaSnapshotMagic = "STVSQL02"
const replicaSnapshotHeader = 24 + sha256.Size

func encodeReplicaSnapshot(path string, incarnation uint64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || uint64(info.Size()) > uint64(int(^uint(0)>>1)-replicaSnapshotHeader) {
		return nil, errors.New("ledger: invalid snapshot database size")
	}
	raw := make([]byte, replicaSnapshotHeader+int(info.Size()))
	copy(raw, replicaSnapshotMagic)
	binary.BigEndian.PutUint64(raw[8:16], incarnation)
	binary.BigEndian.PutUint64(raw[16:24], uint64(info.Size()))
	if _, err := io.ReadFull(file, raw[replicaSnapshotHeader:]); err != nil {
		return nil, fmt.Errorf("read snapshot database: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write(raw[:24])
	_, _ = hash.Write(raw[replicaSnapshotHeader:])
	copy(raw[24:replicaSnapshotHeader], hash.Sum(nil))
	return raw, nil
}

func decodeReplicaSnapshot(raw []byte) (uint64, []byte, error) {
	if len(raw) <= replicaSnapshotHeader || string(raw[:8]) != replicaSnapshotMagic {
		return 0, nil, errors.New("ledger: invalid replica snapshot format")
	}
	incarnation := binary.BigEndian.Uint64(raw[8:16])
	if incarnation == 0 || binary.BigEndian.Uint64(raw[16:24]) != uint64(len(raw)-replicaSnapshotHeader) {
		return 0, nil, errors.New("ledger: invalid replica snapshot identity or length")
	}
	hash := sha256.New()
	_, _ = hash.Write(raw[:24])
	_, _ = hash.Write(raw[replicaSnapshotHeader:])
	if !bytes.Equal(raw[24:replicaSnapshotHeader], hash.Sum(nil)) {
		return 0, nil, errors.New("ledger: replica snapshot checksum differs")
	}
	return incarnation, raw[replicaSnapshotHeader:], nil
}
