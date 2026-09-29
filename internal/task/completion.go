package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

var (
	ErrCompleteRoot      = errors.New("only a chat root task can be completed")
	ErrCompleteState     = errors.New("task must be running or in review")
	ErrCompleteBusy      = errors.New("execution is active or not confirmed idle")
	ErrCompleteChildren  = errors.New("child tasks are not closed")
	ErrCompleteDelivery  = errors.New("results are not durably delivered and acknowledged")
	ErrCompleteAttention = errors.New("a user answer or reconciliation is pending")
	// ErrCompleteConversation comes with ErrCompleteDelivery or
	// ErrCompleteAttention, never alone, when what is pending is the
	// conversation's rather than a task's being checked: a question or a
	// line of another task, or of none. Ending or cancelling a task being
	// checked does not settle it.
	ErrCompleteConversation = errors.New("held by the conversation, not by the tasks checked")
)

func completionTree(root Task, all []Task) []Task {
	selected := map[string]bool{root.ID: true}
	for changed := true; changed; {
		changed = false
		for _, candidate := range all {
			if !selected[candidate.ID] && selected[candidate.Parent] {
				selected[candidate.ID], changed = true, true
			}
		}
	}
	tree := []Task{root}
	for _, candidate := range all {
		if candidate.ID != root.ID && selected[candidate.ID] {
			tree = append(tree, candidate)
		}
	}
	return tree
}

func CompletionBlocker(root Task, all []Task) error {
	if root.Parent != "" || root.Origin != "" || root.PreparedPlan != nil {
		return ErrCompleteRoot
	}
	if root.State != StateRunning && root.State != StateReview {
		return ErrCompleteState
	}
	return completionBlocker(root, completionTree(root, all))
}

// CompletionEligibility indexes the forest once for a read-model snapshot.
// Every root owns a disjoint subtree, so history is not scanned per root.
func CompletionEligibility(all []Task) map[string]bool {
	children := make(map[string][]Task)
	for _, member := range all {
		children[member.Parent] = append(children[member.Parent], member)
	}
	eligible := make(map[string]bool)
	for _, root := range children[""] {
		if root.ID == "" || root.Origin != "" || root.PreparedPlan != nil || (root.State != StateRunning && root.State != StateReview) {
			continue
		}
		tree := []Task{root}
		for i := 0; i < len(tree); i++ {
			tree = append(tree, children[tree[i].ID]...)
		}
		eligible[root.ID] = completionBlocker(root, tree) == nil
	}
	return eligible
}

func completionBlocker(root Task, tree []Task) error {
	if root.Parent != "" || root.Origin != "" || root.PreparedPlan != nil {
		return ErrCompleteRoot
	}
	if root.State != StateRunning && root.State != StateReview {
		return ErrCompleteState
	}
	for _, member := range tree {
		for _, row := range member.Attempts {
			if row.Open() {
				return fmt.Errorf("%w: #%s", ErrCompleteBusy, member.ID)
			}
		}
		if member.ID == root.ID {
			continue
		}
		// A member its owner has closed by hand is as settled as a
		// cancelled one: they looked at the failure and decided.
		if member.Settled() {
			continue
		}
		if !member.State.Terminal() {
			return fmt.Errorf("%w: #%s", ErrCompleteChildren, member.ID)
		}
		// The durable cancellation itself settles work that produced no
		// result. Execution/WAL/queue guards still require physical cleanup;
		// cancellation never invents a successful result or delivery receipt.
		if member.State == StateCancelled && member.Result == nil && member.Delivery == nil {
			continue
		}
		if member.Result == nil || member.Delivery == nil || member.Delivery.State != DeliveryDelivered {
			return fmt.Errorf("%w: #%s", ErrCompleteDelivery, member.ID)
		}
	}
	return nil
}

// CompletionRefused reports whether err is a completion check saying no,
// because something of the task is still unsettled, rather than a failure
// to check at all.
func CompletionRefused(err error) bool {
	for _, refusal := range []error{ErrCompleteRoot, ErrCompleteState, ErrCompleteBusy, ErrCompleteChildren, ErrCompleteDelivery, ErrCompleteAttention} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// CloseChecked ends the tasks as done when their conversation lets go of
// them, not because anyone claimed the work succeeded: a session reset, a
// project switch, a schedule's next run. check runs once for each task, as
// it was and in the order of ids, within the transaction that closes them,
// so they close together or not at all: one the check refuses leaves every
// one of them as it was. The execution epoch is kept, as Advance keeps it.
func (s *Store) CloseChecked(ctx context.Context, ids []string, check func(*ledger.Tx, Task) error) ([]Task, error) {
	if check == nil {
		return nil, errors.New("closing a task requires a completion check")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.draft()
	before := make([]Task, 0, len(ids))
	closed := make([]*Task, 0, len(ids))
	for _, id := range ids {
		stored := next.edit(id)
		if stored == nil {
			return nil, fmt.Errorf("task %s not found", id)
		}
		if !stored.State.CanMoveTo(StateDone) {
			return nil, fmt.Errorf("task %s cannot move %s -> %s", id, stored.State, StateDone)
		}
		before = append(before, *stored.clone())
		stored.State = StateDone
		stored.UpdatedAt = s.now()
		closed = append(closed, stored)
	}
	err := s.replaceRecordsLocked(ctx, next, func(tx *ledger.Tx) error {
		for _, t := range before {
			if err := check(tx, t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(closed))
	for _, stored := range closed {
		out = append(out, *stored.clone())
	}
	return out, nil
}

func (s *Store) CompleteRoot(ctx context.Context, id, channel string, guard func(*ledger.Tx, map[string]bool) error) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, ok := s.data.Tasks[id]
	if !ok || root.Channel != channel {
		return Task{}, fmt.Errorf("task %s not found in this conversation", id)
	}
	if root.Parent != "" || root.Origin != "" || root.PreparedPlan != nil {
		return Task{}, ErrCompleteRoot
	}
	if root.State == StateDone && root.CompletedByUser {
		return *root.clone(), nil
	}
	all := make([]Task, 0, len(s.data.Tasks))
	for _, member := range s.data.Tasks {
		all = append(all, *member)
	}
	tree := completionTree(*root, all)
	if err := completionBlocker(*root, tree); err != nil {
		return Task{}, err
	}
	ids := make(map[string]bool, len(tree))
	next := s.draft()
	for _, member := range tree {
		ids[member.ID] = true
		edited := next.edit(member.ID)
		edited.ExecutionEpoch++
		edited.UpdatedAt = s.now()
	}
	done := next.edit(id)
	done.State = StateDone
	done.CompletedByUser = true
	err := s.replaceAuthorizedLocked(ctx, ExecutionToken{TaskID: id, Epoch: root.ExecutionEpoch}, next, func(tx *ledger.Tx) error {
		if guard == nil {
			return errors.New("task completion requires a durable admission guard")
		}
		return guard(tx, ids)
	})
	if err != nil {
		return Task{}, err
	}
	return *done.clone(), nil
}
