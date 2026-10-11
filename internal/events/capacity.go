package events

import (
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Include unresolved fallback Inbox rows in admission. A provider with no rule
// must not evade backlog limits merely because routing removed its outbox row.
func (s *Service) checkCapacityTx(tx *ledger.Tx, source string, incoming int) error {
	limits := s.Limits.Effective()
	query := `WITH unfinished(id) AS (
		SELECT id FROM bindings WHERE kind=?
		UNION SELECT json_extract(i.data,'$.event_id') FROM bindings AS q JOIN bindings AS i ON i.kind=? AND i.id=q.id WHERE q.kind=?
		UNION SELECT json_extract(data,'$.event_id') FROM bindings WHERE kind=? AND json_extract(data,'$.state') IN ('pending','claimed','accepted','unknown')
	) SELECT count(*),coalesce(sum(CASE WHEN json_extract(e.data,'$.source')=? THEN 1 ELSE 0 END),0) FROM unfinished AS u JOIN bindings AS e ON e.kind=? AND e.id=u.id`
	var total, own int
	if err := tx.QueryRow(query, OutboxKind, InboxKind, InboxOpenKind, DeliveryKind, source, EventKind).Scan(&total, &own); err != nil {
		return err
	}
	if total+incoming > limits.Total || own+incoming > limits.PerSource {
		return fmt.Errorf("%w: unresolved events global=%d/%d source=%d/%d", durable.ErrCapacity, total, limits.Total, own, limits.PerSource)
	}
	return nil
}

// Reactivated events must be hot so unfinished admission indexes retain their
// original source. The typed read verifies and restores the exact frozen fact.
func restoreEventTx(tx *ledger.Tx, id string) (Event, error) {
	event, found, err := durable.Get[Event](tx, EventKind, id)
	if err != nil {
		return Event{}, err
	}
	if !found {
		return Event{}, errors.New("event not found")
	}
	var pointer string
	if err := tx.QueryRow(`SELECT coalesce(json_extract(data,'$._steve_cold_record'),'') FROM bindings WHERE kind=? AND id=?`, EventKind, id).Scan(&pointer); err != nil {
		return Event{}, err
	}
	if pointer != "" {
		if err := tx.PutBinding(EventKind, id, event); err != nil {
			return Event{}, err
		}
	}
	return event, nil
}
