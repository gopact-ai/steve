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
	Outcome string   `json:"outcome,omitempty"`
	Answer  string   `json:"answer,omitempty"`
	Refs    []string `json:"refs,omitempty"`
	Attempt string   `json:"attempt,omitempty"`
}

// Delivery says whether a child's result has reached the conversation
// its parent lives in. Pending is written before the attempt to deliver,
// delivered after it succeeded: a crash between the two is retried, and
// the delivery key keeps the retry from arriving twice.
type Delivery struct {
	State string    `json:"state"` // pending | delivered
	Key   string    `json:"key,omitempty"`
	At    time.Time `json:"at"`
}

const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
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
	t.Delivery = &Delivery{State: state, Key: DeliveryKey(id), At: s.now()}
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
		if t.Delivery != nil && t.Delivery.State == DeliveryDelivered {
			continue
		}
		out[t.Parent] = append(out[t.Parent], t)
	}
	for parent := range out {
		slices.SortFunc(out[parent], func(a, b Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	}
	return out
}
