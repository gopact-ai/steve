package state

import (
	"context"
	"maps"
	"reflect"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ArchiveSessionsChecked archives the entire expected set in one transaction.
// The guard may also commit the new project binding: failed authorization,
// conflicts or persistence then roll back both stores together.
func (s *Store) ArchiveSessionsChecked(ctx context.Context, conversationID string, expected map[string]Session, at string, guard func(*ledger.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !maps.EqualFunc(s.data.Conversations[conversationID].Sessions, expected, func(a, b Session) bool { return reflect.DeepEqual(a, b) }) {
		return ErrSessionChanged
	}
	next := cloneData(s.data)
	for _, id := range slices.Sorted(maps.Keys(expected)) {
		if expected[id].ClosingAt != "" {
			return ErrSessionClosing
		}
		archiveSession(&next, conversationID, id, at)
	}
	return s.replaceCheckedLocked(ctx, next, guard)
}
