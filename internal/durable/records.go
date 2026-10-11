// Package durable supplies typed ledger records to service modules. All writes
// use the ledger's replicated transaction path; it introduces no second store.
package durable

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}

func PageOf[T any](ctx context.Context, book *ledger.Ledger, kind, after string) (Page[T], error) {
	items, next, err := List[T](ctx, book, kind, after, 100)
	return Page[T]{Items: items, Next: next}, err
}

// Key hashes a tuple, so a provider's separators cannot alias another source.
func Key(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func Get[T any](r ledger.Reader, kind, id string) (T, bool, error) {
	var value T
	var raw []byte
	err := r.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	value, err = Decode[T](r, raw, kind, id)
	return value, err == nil, err
}

func Read[T any](ctx context.Context, book *ledger.Ledger, kind, id string) (value T, found bool, err error) {
	err = book.Read(ctx, func(tx *ledger.ReadTx) error {
		value, found, err = Get[T](tx, kind, id)
		return err
	})
	return
}

// List is a bounded keyset page. A cursor is the last returned binding ID.
func List[T any](ctx context.Context, book *ledger.Ledger, kind, after string, limit int) (values []T, next string, err error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	values = []T{}
	err = book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, e := tx.Query(`SELECT id,data FROM bindings WHERE kind=? AND id>? ORDER BY id LIMIT ?`, kind, after, limit+1)
		if e != nil {
			return e
		}
		defer rows.Close()
		last := ""
		for rows.Next() {
			var id string
			var raw []byte
			var value T
			if e := rows.Scan(&id, &raw); e != nil {
				return e
			}
			if len(values) == limit {
				next = last
				break
			}
			value, e = Decode[T](tx, raw, kind, id)
			if e != nil {
				return e
			}
			values = append(values, value)
			last = id
		}
		return rows.Err()
	})
	return
}

func Delete(tx *ledger.Tx, kind, id string) error {
	_, err := tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, kind, id)
	return err
}
