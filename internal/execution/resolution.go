package execution

import "github.com/gopact-ai/steve/internal/task"

// ResolveStopped consumes the identity whose native stop and task accounting
// have both committed. A task's newer epoch is not part of that confirmation.
func (r *Registry) ResolveStopped(attemptID string, token task.ExecutionToken) {
	if attemptID == "" || token.TaskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for s := range r.entries {
		if s.key.AttemptID != attemptID {
			continue
		}
		if s.key.TaskID != token.TaskID || s.token == nil || *s.token != token {
			continue
		}
		if s.joinedLocked() {
			delete(r.entries, s)
		}
	}
}
