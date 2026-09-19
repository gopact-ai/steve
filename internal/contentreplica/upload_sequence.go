package contentreplica

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/gopact-ai/steve/internal/ledger"
)

const uploadClockKind = "content-upload-clock"

type uploadClock struct {
	High uint64 `json:"high"`
}

// The order is part of the exact identity: changing it creates a different
// upload and receipt key, not a way to replay an old promise above a fence.
func uploadSequence(id string) uint64 {
	if !digest(id, 64) {
		return 0
	}
	n, err := strconv.ParseUint(id[:16], 16, 64)
	if err != nil {
		return 0
	}
	return n
}

func readUploadClock(query retentionQuery) (uint64, error) {
	var raw string
	err := query(`SELECT data FROM bindings WHERE kind = ? AND id = 'sequence'`, uploadClockKind).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var clock uploadClock
	if json.Unmarshal([]byte(raw), &clock) != nil || clock.High == 0 {
		return 0, fmt.Errorf("%w: upload allocation clock", ErrIntegrity)
	}
	return clock.High, nil
}

func allocateUpload(tx *ledger.Tx, object Object, targets []string) (Upload, error) {
	high, err := readUploadClock(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return Upload{}, err
	}
	if high == math.MaxUint64 {
		return Upload{}, fmt.Errorf("%w: upload sequence exhausted", ErrInvalid)
	}
	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	u := Upload{ID: fmt.Sprintf("%016x%s", high+1, hex.EncodeToString(nonce[:])), Object: object}
	return u, Reserve(tx, u, targets)
}

// ReleaseUnknown adopts only a release decision, never a new publication.
// This permits explicitly closing an old unknown receipt below the allocation
// clock without reopening a pruned upload's admission.
func ReleaseUnknown(tx *ledger.Tx, upload Upload, targets []string) error {
	if uploadSequence(upload.ID) == 0 || validateObject(upload.Object, math.MaxInt64-1) != nil {
		return ErrInvalid
	}
	targets, err := uploadTargets(targets)
	if err != nil {
		return err
	}
	old, found, err := loadUpload(tx, upload.ID)
	if err != nil {
		return err
	}
	if found {
		if old.Upload != upload || !slices.Equal(old.Targets, targets) {
			return ErrIntegrity
		}
		return AbortUpload(tx, upload.ID)
	}
	high, err := readUploadClock(func(q string, args ...any) retentionRow { return tx.QueryRow(q, args...) })
	if err != nil {
		return err
	}
	if n := uploadSequence(upload.ID); n > high {
		if err := tx.PutBinding(uploadClockKind, "sequence", uploadClock{High: n}); err != nil {
			return err
		}
	}
	u := uploadRecord{Upload: upload, State: "aborted", Targets: targets}
	if err := tx.PutBinding(uploadKind, upload.ID, u); err != nil {
		return err
	}
	return AbortUpload(tx, upload.ID)
}
