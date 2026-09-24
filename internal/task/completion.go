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
