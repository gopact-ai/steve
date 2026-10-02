package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"

	"github.com/gopact-ai/steve/internal/ledger"
)

// RecoveryContextsTx owns the live and archived cwd enumeration. Empty slots
// are not evidence of machine exit; the attempt owner still checks its history.
func RecoveryContextsTx(tx ledger.Reader, node, directory string) ([]Session, error) {
	if directory == "" {
		return nil, nil
	}
	var raw []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='document' AND id='state'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot storedData
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("session state is not one document")
	}
	var result []Session
	add := func(session Session) {
		if session.NodeID == node && session.Workspace != "" && path.Clean(session.Workspace) == path.Clean(directory) {
			result = append(result, session)
		}
	}
	for _, conversation := range snapshot.Conversations {
		for _, session := range conversation.Sessions {
			add(session)
		}
		for _, archived := range conversation.Archived {
			add(archived.Session)
		}
	}
	return result, nil
}

// RetireRecoverySession uses exact native identity, never the current agent
// slot by itself. Its guard persists the episode retirement in the same write.
// Normal successful copy turns are not projected as abandoned attempts.
func (s *Store) RetireRecoverySession(ctx context.Context, node, harness, session, attemptID, at string, guard func(*ledger.Tx) error) error {
	if session == "" || attemptID == "" || guard == nil {
		return errors.New("recovery retirement requires its exact execution")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneData(s.data)
	if next.RetiredContexts == nil {
		next.RetiredContexts = map[string]string{}
	}
	key := nativeContextKey(node, harness, session)
	if owner := next.RetiredContexts[key]; owner != "" && owner != attemptID {
		return errors.New("another execution retired this native context")
	}
	next.RetiredContexts[key] = attemptID
	for id, conversation := range next.Conversations {
		retire := func(one Session) {
			if one.NativeImport != nil {
				next.RetiredContexts[importedContextKey(id, one.AgentID, node, harness, NativeImportFingerprint(one.NativeImport))] = attemptID
			}
		}
		for agent, one := range conversation.Sessions {
			if one.NodeID == node && one.HarnessID == harness && one.UpstreamID == session {
				retire(one)
				archiveSession(&next, id, agent, at)
			}
		}
		for _, archived := range next.Conversations[id].Archived {
			one := archived.Session
			if one.NodeID == node && one.HarnessID == harness && one.UpstreamID == session {
				retire(one)
			}
		}
	}
	if err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		if err := guard(tx); err != nil {
			return err
		}
		if reflect.DeepEqual(next, s.data) {
			return nil
		}
		return tx.PutBinding("document", "state", next)
	}); err != nil {
		return err
	}
	s.data = next
	return nil
}
