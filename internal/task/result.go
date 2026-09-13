package task

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Result is what a delegated child left when it ended, kept on the task
// so its parent can be told after any restart — not only while the
// delegating service still remembers the child in memory.
type Result struct {
	Outcome Outcome  `json:"outcome,omitempty"`
	Answer  string   `json:"answer,omitempty"`
	Refs    []string `json:"refs,omitempty"`
	Attempt string   `json:"attempt,omitempty"`
}

// Delivery says whether a child's result has reached the conversation
// its parent lives in. Pending is written before the attempt to deliver,
// delivered after it succeeded: a crash between the two is retried, and
// the delivery key keeps the retry from arriving twice.
type Delivery struct {
	State         string    `json:"state"` // pending | delivered | suppressed | uncertain
	Key           string    `json:"key,omitempty"`
	At            time.Time `json:"at"`
	Attempts      int       `json:"attempts,omitempty"`
	Error         string    `json:"error,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

const (
	DeliveryPending    = "pending"
	DeliveryDelivered  = "delivered"
	DeliverySuppressed = "suppressed"
	DeliveryUncertain  = "uncertain"
)

// DeliveryKey names one child's delivery for good.
func DeliveryKey(childID string) string { return "deliver:" + childID }

// Finished says the task has ended one way or another, so a result of
// it can be final.
func (t Task) Finished() bool {
	return t.State == StateDone || t.State == StateFailed || t.State == StateCancelled
}

// Delegated says the task was opened by another task's delegation.
func (t Task) Delegated() bool { return strings.HasPrefix(t.Origin, "delegate:") }

// SetResult records how a delegated child ended.
func (s *Store) SetResult(id string, r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Tasks[id]; !ok {
		return fmt.Errorf("task %s not found", id)
	}
	next := s.clone()
	t := next.Tasks[id]
	r.Refs = slices.Clone(r.Refs)
	t.Result = &r
	t.UpdatedAt = s.now()
	return s.replaceLocked(next)
}

// SetDelivery moves a child's delivery state.
func (s *Store) SetDelivery(id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Tasks[id]; !ok {
		return fmt.Errorf("task %s not found", id)
	}
	next := s.clone()
	t := next.Tasks[id]
	if t.Delivery == nil {
		t.Delivery = &Delivery{Key: DeliveryKey(id)}
	}
	t.Delivery.State, t.Delivery.At = state, s.now()
	t.Delivery.Error, t.Delivery.NextAttemptAt = "", time.Time{}
	return s.replaceLocked(next)
}

// Undelivered lists finished delegated children whose result has not
// reached their parent's conversation, grouped under their parents.
func (s *Store) Undelivered() map[string][]Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]Task{}
	for _, stored := range s.data.Tasks {
		t := *stored.clone()
		if !t.Delegated() || !t.Finished() || t.Result == nil || t.Parent == "" {
			continue
		}
		if t.Delivery != nil && (t.Delivery.State == DeliveryDelivered || t.Delivery.State == DeliverySuppressed) {
			continue
		}
		out[t.Parent] = append(out[t.Parent], t)
	}
	for parent := range out {
		slices.SortFunc(out[parent], deliveryOrder)
	}
	return out
}

func deliveryOrder(a, b Task) int {
	if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
		return order
	}
	return strings.Compare(a.ID, b.ID)
}

func sameDelivery(a, b *Delivery) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
