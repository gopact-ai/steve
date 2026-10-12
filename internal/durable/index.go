package durable

import (
	"context"
	"github.com/gopact-ai/steve/internal/ledger"
)

// PageByIndex reads a bounded page from a domain-maintained pending index.
// Records and index changes commit together; resolved history is not scanned.
func PageByIndex[T any](ctx context.Context, book *ledger.Ledger, index, kind, after string) (page Page[T], err error) {
	page.Items = []T{}
	err = book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, e := tx.Query(`SELECT r.id,r.data FROM bindings AS q JOIN bindings AS r ON r.id=q.id AND r.kind=? WHERE q.kind=? AND q.id>? ORDER BY q.id LIMIT 101`, kind, index, after)
		if e != nil {
			return e
		}
		defer rows.Close()
		last := ""
		for rows.Next() {
			var id string
			var raw []byte
			if e := rows.Scan(&id, &raw); e != nil {
				return e
			}
			if len(page.Items) == 100 {
				page.Next = last
				break
			}
			value, e := Decode[T](tx, raw, kind, id)
			if e != nil {
				return e
			}
			page.Items = append(page.Items, value)
			last = id
		}
		return rows.Err()
	})
	return
}
