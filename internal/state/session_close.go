package state

import (
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrSessionClosing = errors.New("session close is pending; retry /new to finish it")

// BeginSessionCloseChecked commits authority before any irreversible close.
// A retry retains the original intent; it cannot replace the session or stamp.
func (s *Store) BeginSessionCloseChecked(ctx context.Context, expected Session, at string, guard func(*ledger.Tx) error) (Session, error) {
	if expected.ConversationID == "" || expected.AgentID == "" || expected.UpstreamID == "" || at == "" {
		return Session{}, errors.New("session close requires its exact live identity and timestamp")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.data.Conversations[expected.ConversationID].Sessions[expected.AgentID]
	if !found || !reflect.DeepEqual(current, expected) {
		return Session{}, ErrSessionChanged
	}
	if current.ClosingAt != "" {
		return current, nil
	}
	next := cloneData(s.data)
	current.ClosingAt = at
	next.Conversations[expected.ConversationID].Sessions[expected.AgentID] = current
	if err := s.replaceCheckedLocked(ctx, next, guard); err != nil {
		return Session{}, err
	}
	return current, nil
}

// FinishSessionClose settles the exact approved action, without re-authorizing
// it after the native side effect. Failed persistence keeps the intent fenced;
// retrying the same close (including an absent reply) can finish the archive.
func (s *Store) FinishSessionClose(ctx context.Context, expected Session, owed *OwedClose) error {
	if expected.ClosingAt == "" {
		return errors.New("session close has no committed intent")
	}
	if owed != nil && (owed.TaskID == "" || owed.AttemptID == "" || !owed.names(expected)) {
		return errors.New("session close has no matching execution obligation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.data.Conversations[expected.ConversationID].Sessions[expected.AgentID]
	if !found || !reflect.DeepEqual(current, expected) {
		return ErrSessionChanged
	}
	next := cloneData(s.data)
	current.ClosingAt = ""
	next.Conversations[expected.ConversationID].Sessions[expected.AgentID] = current
	archiveSession(&next, expected.ConversationID, expected.AgentID, expected.ClosingAt)
	if owed != nil {
		pending := *owed
		pending.OwedAt = expected.ClosingAt
		next.OwedCloses = slices.DeleteFunc(next.OwedCloses, func(earlier OwedClose) bool { return earlier.names(expected) })
		next.OwedCloses = append(next.OwedCloses, pending)
	}
	return s.replaceCheckedLocked(ctx, next, func(*ledger.Tx) error { return nil })
}
