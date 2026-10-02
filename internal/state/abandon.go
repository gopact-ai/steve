package state

import (
	"context"
	"errors"
	"reflect"

	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrAbandonedContext = errors.New("the abandoned native context cannot be restored")

func nativeContextKey(node, harness, upstream string) string {
	return node + "\x00" + harness + "\x00" + upstream
}

// ProjectAbandonedSession retires only the named native context. It can be
// repeated after a crash without archiving a newer session in the same slot.
func (s *Store) ProjectAbandonedSession(ctx context.Context, conversation, agent string, owed OwedClose, closeNeeded bool, imported string, guard func(*ledger.Tx) error) error {
	if guard == nil || owed.AttemptID == "" || owed.TaskID == "" {
		return errors.New("abandoned session needs its original execution")
	}
	// A lost open is stopped by its original command. No native identity is
	// invented for a close or an archive when no binding was ever received.
	if owed.UpstreamID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	key := nativeContextKey(owed.NodeID, owed.HarnessID, owed.UpstreamID)
	if next.RetiredContexts == nil {
		next.RetiredContexts = map[string]string{}
	}
	if previous := next.RetiredContexts[key]; previous != "" && previous != owed.AttemptID {
		return errors.New("native context was retired for another execution")
	}
	next.RetiredContexts[key] = owed.AttemptID
	if imported != "" {
		next.RetiredContexts[importedContextKey(conversation, agent, owed.NodeID, owed.HarnessID, imported)] = owed.AttemptID
	}
	current := next.Conversations[conversation]
	if live, ok := current.Sessions[agent]; ok && owed.names(live) {
		archiveSession(&next, conversation, agent, owed.OwedAt)
	}
	for id, source := range next.Conversations {
		for index := range source.Archived {
			if owed.names(source.Archived[index].Session) {
				source.Archived[index].AbandonedAttempt = owed.AttemptID
			}
		}
		next.Conversations[id] = source
	}
	if closeNeeded {
		found := false
		for _, pending := range next.OwedCloses {
			if pending.NodeID == owed.NodeID && pending.HarnessID == owed.HarnessID && pending.UpstreamID == owed.UpstreamID {
				if pending.AttemptID != owed.AttemptID || pending.TaskID != owed.TaskID || pending.NativeContext != owed.NativeContext {
					return errors.New("another execution owns this session's close")
				}
				found = true
			}
		}
		if !found {
			next.OwedCloses = append(next.OwedCloses, owed)
		}
	}
	if reflect.DeepEqual(next, s.data) {
		return nil
	}
	if err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		if err := guard(tx); err != nil {
			return err
		}
		return tx.PutBinding("document", "state", next)
	}); err != nil {
		return err
	}
	s.data = next
	return nil
}
