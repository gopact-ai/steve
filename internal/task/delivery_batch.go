package task

import (
	"fmt"
	"slices"
	"time"
)

// PrepareDeliveries freezes each message's membership before calling a channel.
// A later child gets a new key, even if an earlier message lost its receipt.
// The task records and their batch keys commit together.
func (s *Store) PrepareDeliveries(parent string, candidates []string) ([][]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Tasks[parent]
	if !ok {
		return nil, fmt.Errorf("task %s not found", parent)
	}
	if p.State != StateRunning {
		return nil, nil
	}
	next := s.draft()
	var fresh []Task
	groups := map[string][]Task{}
	seen := map[string]bool{}
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		t, ok := next.find(id)
		if !ok || t.Parent != parent || !t.Delegated() || !t.Finished() || t.Result == nil {
			continue
		}
		if t.Delivery != nil && t.Delivery.State != DeliveryPending && t.Delivery.State != DeliveryQueued {
			continue
		}
		if t.Delivery == nil || t.Delivery.Key == "" {
			fresh = append(fresh, *t)
			continue
		}
		if t.Delivery.State == DeliveryPending || t.Delivery.State == DeliveryQueued {
			groups[t.Delivery.Key] = append(groups[t.Delivery.Key], *t.clone())
		}
	}
	if len(fresh) > 0 {
		slices.SortFunc(fresh, deliveryOrder)
		key := DeliveryKey(fresh[0].ID)
		for _, child := range fresh {
			t := next.edit(child.ID)
			t.Delivery = &Delivery{State: DeliveryPending, Key: key, At: s.now()}
			groups[key] = append(groups[key], *t.clone())
		}
		if err := s.replaceLocked(next); err != nil {
			return nil, err
		}
	}
	var out [][]Task
	for _, children := range groups {
		slices.SortFunc(children, deliveryOrder)
		out = append(out, children)
	}
	slices.SortFunc(out, func(a, b []Task) int { return deliveryOrder(a[0], b[0]) })
	return out, nil
}

// StartDelivery persists the send before any side effect. Only a channel with
// durable deduplication can safely retry after a process dies without a receipt.
func (s *Store) StartDelivery(ids []string, replaySafe bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.draft()
	for _, id := range ids {
		t := next.edit(id)
		if t == nil || t.Delivery == nil || t.Delivery.State != DeliveryPending && t.Delivery.State != DeliveryQueued {
			return fmt.Errorf("task %s has no pending delivery", id)
		}
		d := t.Delivery
		if d.State == DeliveryQueued {
			continue
		}
		d.Attempts++
		d.At = s.now()
		d.Error = ""
		d.NextAttemptAt = d.At.Add(deliveryRetryDelay(d.Attempts))
		if !replaySafe {
			d.State = DeliveryUncertain
			d.Error = "Delivery started without a durable receipt; automatic replay is disabled"
			d.NextAttemptAt = time.Time{}
		}
	}
	return s.replaceLocked(next)
}

// RecordDelivery commits the outcome for every member of one fixed message.
// A result consumed by an inline waiter must not be made pending again.
func (s *Store) RecordDelivery(ids []string, state, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.draft()
	for _, id := range ids {
		t := next.edit(id)
		if t == nil || t.Delivery == nil {
			return fmt.Errorf("task %s has no prepared delivery", id)
		}
		if t.Delivery.State == DeliveryDelivered || t.Delivery.State == DeliverySuppressed {
			continue
		}
		d := t.Delivery
		d.State, d.At, d.Error = state, s.now(), detail
		d.NextAttemptAt = time.Time{}
		if state == DeliveryQueued {
			d.NextAttemptAt = d.At.Add(5 * time.Second)
		}
		if state == DeliveryPending {
			d.NextAttemptAt = d.At.Add(deliveryRetryDelay(d.Attempts))
		}
	}
	return s.replaceLocked(next)
}

func deliveryRetryDelay(attempts int) time.Duration {
	delay := 5 * time.Second
	for i := 1; i < attempts && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 5*time.Minute)
}
