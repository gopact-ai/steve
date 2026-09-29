package state

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

// ArchiveSessionOwingClose is ArchiveSession for a session whose close
// has not reached its node yet.
func (s *Store) ArchiveSessionOwingClose(conversationID, agentID, at string, owed OwedClose) error {
	return nil
}

// OwedCloses lists the sessions still owed a close.
func (s *Store) OwedCloses() []OwedClose {
	return nil
}

// SettleOwedClose forgets owed once its close is no longer owed.
func (s *Store) SettleOwedClose(owed OwedClose) error {
	return nil
}
