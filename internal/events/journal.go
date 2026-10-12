package events

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/eventlog"
	"github.com/gopact-ai/steve/internal/ledger"
)

// JournalEntry is a read-only envelope. Payloads are fetched on inspection,
// rather than multiplying large external content across every polling page.
type JournalEntry struct {
	ID        string               `json:"id"`
	Source    string               `json:"source"`
	Key       string               `json:"key"`
	Type      string               `json:"type"`
	Subject   string               `json:"subject,omitempty"`
	Causation string               `json:"causation,omitempty"`
	Depth     int                  `json:"depth"`
	At        time.Time            `json:"at"`
	Routed    bool                 `json:"routed"`
	Contract  *eventlog.Definition `json:"contract,omitempty"`
}

// Journal reads envelopes and outbox evidence in the same snapshot. Routed
// means fanout committed, never that a consumer completed its external effect.
// The existing typed cold reader keeps archived events discoverable as well.
func (s *Service) Journal(ctx context.Context, after string) (page durable.Page[JournalEntry], err error) {
	page.Items = []JournalEntry{}
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, err := tx.Query(`SELECT e.id,e.data,NOT EXISTS(SELECT 1 FROM bindings AS o WHERE o.kind=? AND o.id=e.id) FROM bindings AS e WHERE e.kind=? AND e.id>? ORDER BY e.id LIMIT 33`, OutboxKind, EventKind, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		last := ""
		for rows.Next() {
			var id string
			var raw []byte
			var routed bool
			if err := rows.Scan(&id, &raw, &routed); err != nil {
				return err
			}
			if len(page.Items) == 32 {
				page.Next = last
				break
			}
			item, err := durable.Decode[JournalEntry](tx, raw, EventKind, id)
			if err != nil {
				return err
			}
			item.Routed = routed
			page.Items = append(page.Items, item)
			last = id
		}
		return rows.Err()
	})
	return
}
