package events

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
)

type ActivitySnapshot struct {
	Recent        []JournalEntry   `json:"recent"`
	Attention     []AttentionEntry `json:"attention"`
	MoreAttention bool             `json:"more_attention"`
}
type AttentionEntry struct {
	ID      string    `json:"id"`
	EventID string    `json:"event_id"`
	Type    string    `json:"type"`
	Subject string    `json:"subject,omitempty"`
	Source  string    `json:"source"`
	Rule    string    `json:"rule"`
	State   string    `json:"state"`
	Mode    string    `json:"mode"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// Activity is a small dashboard projection. Recent order comes from immutable
// intake receipts, so compression and random IDs cannot promote old history.
// Attention comes from the open Inbox index, independently of that recent window.
func (s *Service) Activity(ctx context.Context) (out ActivitySnapshot, err error) {
	out.Recent = []JournalEntry{}
	out.Attention = []AttentionEntry{}
	err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, err := tx.Query(`SELECT json_extract(data,'$.adapter_receipt') FROM bindings WHERE kind=? AND `+flowInputKind+`='event' AND `+flowEventTime+` IS NOT NULL ORDER BY `+flowEventTime+` DESC,id DESC LIMIT 32`, ingress.RecordKind)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			item, found, err := durable.Get[JournalEntry](tx, EventKind, id)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("event activity receipt has no original event")
			}
			var routing int
			if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind=? AND id=?`, OutboxKind, id).Scan(&routing); err != nil {
				return err
			}
			item.Routed = routing == 0
			out.Recent = append(out.Recent, item)
		}
		sort.Slice(out.Recent, func(i, j int) bool {
			if out.Recent[i].At.Equal(out.Recent[j].At) {
				return out.Recent[i].ID > out.Recent[j].ID
			}
			return out.Recent[i].At.After(out.Recent[j].At)
		})
		// Start with the domain's open index. A resolved-history flood must
		// not hide or require scanning past an older unresolved delivery.
		var attention []Inbox
		rows, err = tx.Query(`SELECT i.id,i.data FROM bindings AS q CROSS JOIN bindings AS i WHERE q.kind=? AND i.kind=? AND i.id=q.id AND (CASE WHEN json_valid(i.data) THEN json_extract(i.data,'$.reason') END) IN (?,?) ORDER BY json_extract(i.data,'$.at') DESC,i.id DESC LIMIT 5`, InboxOpenKind, InboxKind, StateFailed, StateUnknown)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			item, err := durable.Decode[Inbox](tx, raw, InboxKind, id)
			if err != nil {
				rows.Close()
				return err
			}
			attention = append(attention, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		sort.Slice(attention, func(i, j int) bool {
			if attention[i].At.Equal(attention[j].At) {
				return attention[i].ID > attention[j].ID
			}
			return attention[i].At.After(attention[j].At)
		})
		out.MoreAttention = len(attention) > 4
		if out.MoreAttention {
			attention = attention[:4]
		}
		for _, inbox := range attention {
			delivery, found, err := durable.Get[Delivery](tx, DeliveryKind, inbox.DeliveryID)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("open delivery attention has no original delivery")
			}
			event, found, err := durable.Get[JournalEntry](tx, EventKind, delivery.EventID)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("delivery attention has no original event")
			}
			out.Attention = append(out.Attention, AttentionEntry{ID: delivery.ID, EventID: event.ID, Type: event.Type, Subject: event.Subject, Source: event.Source, Rule: delivery.Rule.Name, State: delivery.State, Mode: delivery.Rule.Mode, Detail: delivery.Detail, At: delivery.UpdatedAt})
		}
		return nil
	})
	return
}
