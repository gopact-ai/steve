package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

var errCloseStateBusy = errors.New("state is busy; retry close reconciliation")

// OwedClosesContext takes a snapshot without waiting behind another state
// write. A busy store leaves the closes for the next reconciliation pass.
func (s *Store) OwedClosesContext(ctx context.Context) ([]OwedClose, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.mu.TryLock() {
		return nil, errCloseStateBusy
	}
	defer s.mu.Unlock()
	return slices.Clone(s.data.OwedCloses), nil
}

// SettleOwedCloses forgets exactly the completed obligations in one write.
// It never waits for another state or ledger writer; a refused write leaves
// the complete batch owed. Replacements of the same session are not matched.
func (s *Store) SettleOwedCloses(ctx context.Context, owed ...OwedClose) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.mu.TryLock() {
		return errCloseStateBusy
	}
	defer s.mu.Unlock()
	matches := func(current OwedClose) bool { return slices.Contains(owed, current) }
	if !slices.ContainsFunc(s.data.OwedCloses, matches) {
		return nil
	}
	next := cloneData(s.data)
	next.OwedCloses = slices.DeleteFunc(next.OwedCloses, matches)
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := s.doc.SaveContext(ctx, raw); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	s.data = next
	return nil
}
