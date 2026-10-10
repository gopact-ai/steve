package state

import (
	"errors"
	"reflect"
	"slices"
)

var ErrSessionChanged = errors.New("session changed while its retirement was in flight")

// RetireSession removes only the session that was actually closed. An
// undispatched managed close archives that same slot and its exact obligation
// in one durable write; a rejected write leaves the live slot untouched.
func (s *Store) RetireSession(expected Session, at string, owed *OwedClose) error {
	if expected.ConversationID == "" || expected.AgentID == "" {
		return errors.New("session retirement requires its conversation and agent")
	}
	if owed != nil && (owed.UpstreamID == "" || owed.HarnessID == "" || owed.TaskID == "" || owed.AttemptID == "" || at == "" || owed.OwedAt != at || !owed.names(expected)) {
		return errors.New("session retirement has no matching close obligation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.data.Conversations[expected.ConversationID].Sessions[expected.AgentID]
	if !found || !reflect.DeepEqual(current, expected) {
		return ErrSessionChanged
	}
	next := cloneData(s.data)
	if owed != nil {
		next.OwedCloses = slices.DeleteFunc(next.OwedCloses, func(earlier OwedClose) bool { return earlier.names(expected) })
		next.OwedCloses = append(next.OwedCloses, *owed)
		archiveSession(&next, expected.ConversationID, expected.AgentID, at)
	} else {
		conversation := next.Conversations[expected.ConversationID]
		delete(conversation.Sessions, expected.AgentID)
		next.Conversations[expected.ConversationID] = conversation
	}
	return s.replaceLocked(next)
}
