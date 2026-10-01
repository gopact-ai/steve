package state

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ProjectAbandonedOpen retires the preinstalled import, not an invented native
// session. A newer empty slot or another import snapshot is never captured.
func (s *Store) ProjectAbandonedOpen(ctx context.Context, conversation, agent, node, harness, attemptID, slotState, fingerprint, imported string, guard func(*ledger.Tx) error) error {
	if guard == nil || attemptID == "" {
		return errors.New("pending open retirement needs its original decision")
	}
	if slotState == "absent" || slotState == "different" {
		return s.book.Update(ctx, guard)
	}
	if slotState != "original" || fingerprint == "" || imported == "" {
		return errors.New("pending open retirement has no exact imported slot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	key := importedContextKey(conversation, agent, node, harness, imported)
	if next.RetiredContexts == nil {
		next.RetiredContexts = map[string]string{}
	}
	if prior := next.RetiredContexts[key]; prior != "" && prior != attemptID {
		return errors.New("imported context was retired by another execution")
	}
	next.RetiredContexts[key] = attemptID
	thread := next.Conversations[conversation]
	if current, ok := thread.Sessions[agent]; ok && current.UpstreamID == "" && sessionFingerprint(current) == fingerprint {
		delete(thread.Sessions, agent)
		next.Conversations[conversation] = thread
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
