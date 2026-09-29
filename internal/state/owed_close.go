package state

import (
	"errors"
	"slices"
)

// OwedClose is a native session a node still owes a close: the
// conversation let go of it while the request to close it could not reach
// the node. It names the execution that last ran in the session, so a
// close sent later reaches that session and nothing that took it up since.
type OwedClose struct {
	NodeID     string `json:"node_id"`
	HarnessID  string `json:"harness_id"`
	UpstreamID string `json:"upstream_id"`
	// NativeContext is the agent's own id for the context the session
	// holds, as the execution recorded it.
	NativeContext string `json:"native_context,omitempty"`
	TaskID        string `json:"task_id"`
	AttemptID     string `json:"attempt_id"`
	OwedAt        string `json:"owed_at"`
}

// names reports whether session is the native session owed names.
func (owed OwedClose) names(session Session) bool {
	return owed.NodeID == session.NodeID && owed.HarnessID == session.HarnessID && owed.UpstreamID == session.UpstreamID
}

// ArchiveSessionOwingClose is ArchiveSession for a session whose close
// has not reached its node yet. The archive and the close owed are one
// write: a coordinator that takes over finds both or neither. owed must
// name the live session and the execution that ran in it; a close owed
// before for the same session is replaced.
func (s *Store) ArchiveSessionOwingClose(conversationID, agentID, at string, owed OwedClose) error {
	if owed.UpstreamID == "" || owed.HarnessID == "" || owed.TaskID == "" || owed.AttemptID == "" {
		return errors.New("a close owed must name the native session and the execution that ran in it")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.data.Conversations[conversationID].Sessions[agentID]
	if !ok {
		return errors.New("no live session to archive owing its close")
	}
	if !owed.names(session) {
		return errors.New("the close owed names another session than the one archived")
	}
	next := cloneData(s.data)
	next.OwedCloses = slices.DeleteFunc(next.OwedCloses, func(earlier OwedClose) bool { return earlier.names(session) })
	next.OwedCloses = append(next.OwedCloses, owed)
	archiveSession(&next, conversationID, agentID, at)
	return s.replaceLocked(next)
}

// OwedCloses lists the sessions still owed a close, oldest first.
func (s *Store) OwedCloses() []OwedClose {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data.OwedCloses)
}

// SettleOwedClose forgets owed once its close is no longer owed. A close
// owed since for the same session under another execution is kept.
func (s *Store) SettleOwedClose(owed OwedClose) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.data.OwedCloses, owed) {
		return nil
	}
	next := cloneData(s.data)
	next.OwedCloses = slices.DeleteFunc(next.OwedCloses, func(earlier OwedClose) bool { return earlier == owed })
	return s.replaceLocked(next)
}
