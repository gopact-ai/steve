package durable

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const ColdKind = "durable-cold-record"
const MaxRecordBytes = 4 << 20

// A cold pointer retains the original kind and ID. Cold data is still in the
// same replicated ledger; compression never removes identity or receipts.
type coldPointer struct {
	ID     string `json:"_steve_cold_record"`
	Digest string `json:"_steve_cold_digest"`
}
type ColdRecord struct {
	Kind   string    `json:"kind"`
	ID     string    `json:"record_id"`
	Digest string    `json:"digest"`
	Data   []byte    `json:"gzip"`
	Bytes  int       `json:"bytes"`
	At     time.Time `json:"at"`
}

func Decode[T any](r ledger.Reader, raw []byte, expected ...string) (value T, err error) {
	var pointer coldPointer
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		if err = json.Unmarshal(raw, &pointer); err != nil {
			return value, err
		}
	}
	if pointer.ID != "" {
		var encoded []byte
		if err = r.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, ColdKind, pointer.ID).Scan(&encoded); err != nil {
			return value, err
		}
		var cold ColdRecord
		if err = json.Unmarshal(encoded, &cold); err != nil {
			return value, err
		}
		if cold.Digest != pointer.Digest || Key(cold.Kind, cold.ID) != pointer.ID {
			return value, errors.New("durable: cold record identity or digest mismatch")
		}
		if len(expected) == 2 && (cold.Kind != expected[0] || cold.ID != expected[1]) {
			return value, errors.New("durable: cold pointer references a different record")
		}
		reader, openErr := gzip.NewReader(bytes.NewReader(cold.Data))
		if openErr != nil {
			return value, openErr
		}
		raw, err = io.ReadAll(io.LimitReader(reader, MaxRecordBytes+1))
		closeErr := reader.Close()
		if err != nil {
			return value, err
		}
		if closeErr != nil {
			return value, closeErr
		}
		if len(raw) > MaxRecordBytes || len(raw) != cold.Bytes || Key(string(raw)) != pointer.Digest {
			return value, errors.New("durable: corrupt cold record")
		}
	}
	err = json.Unmarshal(raw, &value)
	return value, err
}

// ArchiveTx is lossless and idempotent. Only domain-selected terminal records
// may call it; active records and uncertain effects have no TTL.
func ArchiveTx(tx *ledger.Tx, kind, id string) (saved int, err error) {
	var raw []byte
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, kind, id).Scan(&raw); err != nil {
		return 0, err
	}
	var pointer coldPointer
	if err := json.Unmarshal(raw, &pointer); err != nil {
		return 0, err
	}
	if pointer.ID != "" || len(raw) < 512 {
		return 0, nil
	}
	if len(raw) > MaxRecordBytes {
		// Oversized legacy output remains lossless hot evidence. It must not
		// block later eligible records in the bounded compaction cursor.
		return 0, nil
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(raw); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	cold := ColdRecord{Kind: kind, ID: id, Digest: Key(string(raw)), Data: compressed.Bytes(), Bytes: len(raw), At: time.Now().UTC()}
	coldID := Key(kind, id)
	encoded, err := json.Marshal(cold)
	if err != nil {
		return 0, err
	}
	stub := coldPointer{ID: coldID, Digest: cold.Digest}
	stubRaw, _ := json.Marshal(stub)
	// Base64 and pointer overhead are part of the actual storage cost.
	if len(encoded)+len(stubRaw) >= len(raw) {
		return 0, nil
	}
	if err := tx.PutBinding(ColdKind, coldID, cold); err != nil {
		return 0, err
	}
	if err := tx.PutBinding(kind, id, stub); err != nil {
		return 0, err
	}
	return len(raw) - len(encoded) - len(stubRaw), nil
}

// SweepCold removes only an orphaned compressed copy after a domain updates or
// deletes the hot binding. A referenced cold copy is never discarded.
func SweepCold(ctx context.Context, book *ledger.Ledger, after string, limit int) (string, error) {
	items, next, err := List[ColdRecord](ctx, book, ColdKind, after, limit)
	if err != nil {
		return after, err
	}
	for _, cold := range items {
		err = book.Update(ctx, func(tx *ledger.Tx) error {
			var raw []byte
			err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, cold.Kind, cold.ID).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var pointer coldPointer
			if len(raw) > 0 && json.Unmarshal(raw, &pointer) != nil {
				return errors.New("durable: invalid retained binding")
			}
			if pointer.ID == Key(cold.Kind, cold.ID) {
				return nil
			}
			return Delete(tx, ColdKind, Key(cold.Kind, cold.ID))
		})
		if err != nil {
			return after, err
		}
	}
	return next, nil
}
