package node

import (
	"github.com/gopact-ai/steve/internal/nodewire"
)

// resumeSourceLocked follows the exclusive native-context handoff chain. A
// stopped process and settled receipts permit a new input, never an old replay.
// s.mu also serializes this claim with open cancellation tombstones.
func (s *SessionService) resumeSourceLocked(req nodewire.SessionRequest, target string) (*sessionRecord, error) {
	id := req.ID
	seen := map[string]bool{}
	for !seen[id] {
		seen[id] = true
		record, exists, err := s.readRecord(id)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, sessionError("unavailable", "original native context is not recorded")
		}
		if err := validateResumeSource(req, record); err != nil {
			return nil, err
		}
		if record.State.Binding == req.Binding {
			if record.OpenID != req.CommandID {
				return nil, sessionError("conflict", "archived observation must identify the original open")
			}
			return nil, nil // Observation of the original execution stays cold.
		}
		if record.OpenCancelled || record.UpstreamID == "" || !record.State.ProcessStopped || (record.State.State != nodewire.SessionInterrupted && record.State.State != nodewire.SessionClosed) {
			return nil, sessionError("uncertain", "original native process must be confirmed stopped before resuming context")
		}
		// An interrupted turn is not an unknown one. What the next
		// execution may not build on is a command whose outcome the agent
		// never reported, so the check is settlement, not success: a
		// cancelled turn left the native context exactly as consistent as
		// a completed one, and refusing it would strand the conversation
		// on this node for good.
		store, err := s.recordsStore()
		if err != nil {
			return nil, err
		}
		if err := store.resumable(id); err != nil {
			return nil, err
		}
		if record.ResumeTarget == "" || record.ResumeTarget == target {
			return &record, nil
		}
		if s.sessions[record.ResumeTarget] != nil {
			return nil, sessionError("uncertain", "native context handoff still has an owned session requiring reconciliation")
		}
		next, exists, err := s.readRecord(record.ResumeTarget)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, sessionError("uncertain", "original context has an unfinished resume reservation")
		}
		if next.OpenCancelled && next.State.ProcessStopped {
			return &record, nil // That exact open can no longer start.
		}
		id = record.ResumeTarget
	}
	return nil, sessionError("unavailable", "native context handoff cycle requires reconciliation")
}

// validateResumeSource checks identity without rebinding or recording process exit.
func validateResumeSource(req nodewire.SessionRequest, record sessionRecord) error {
	if record.ClusterID != req.Authority.ClusterID || req.Authority.CoordinatorEpoch < record.Authority.CoordinatorEpoch || req.Authority.WriterGeneration < record.Authority.WriterGeneration || (req.Authority.CoordinatorEpoch == record.Authority.CoordinatorEpoch && req.Authority.CoordinatorNodeID != record.Authority.CoordinatorNodeID) {
		return sessionError("forbidden", "stale native context authority")
	}
	before, after := record.State.Binding, req.Binding
	if before.ProjectID != after.ProjectID || before.SessionID != after.SessionID || before.NodeID != after.NodeID || before.NativeImportID != after.NativeImportID || before.PluginRuntimeID != after.PluginRuntimeID || record.State.Harness != req.Harness || record.ConfigHash != sessionConfigHash(req) {
		return sessionError("forbidden", "native context differs from the admitted session or configuration")
	}
	return nil
}
