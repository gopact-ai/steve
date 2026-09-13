package state

import "errors"

// InstallNativeSession initializes a new conversation atomically with its
// active agent. An identical retry never overwrites a subsequently used session.
func (s *Store) InstallNativeSession(session Session) error {
	if session.NativeImport == nil || session.ConversationID == "" || session.AgentID == "" {
		return errors.New("native import needs a selected session and conversation")
	}
	if err := session.NativeImport.Validate(session.HarnessID, session.Workspace); err != nil {
		return err
	}
	session.NativeImport = session.NativeImport.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, exists := s.data.Conversations[session.ConversationID]; exists {
		old, ok := previous.Sessions[session.AgentID]
		if ok && old.NativeImport != nil && *old.NativeImport == *session.NativeImport && old.NodeID == session.NodeID && old.ProjectID == session.ProjectID && old.ProjectVersion == session.ProjectVersion {
			return nil
		}
		return errors.New("native import command already belongs to another conversation binding")
	}
	next := cloneData(s.data)
	next.Conversations[session.ConversationID] = Conversation{ActiveAgent: session.AgentID, Sessions: map[string]Session{session.AgentID: session}}
	return s.replaceLocked(next)
}
