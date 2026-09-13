package execution

import "github.com/gopact-ai/steve/internal/task"

func (r *Registry) WhileTaskIdle(id string, complete func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for scope := range r.entries {
		if scope.key.TaskID == id {
			return task.ErrCompleteBusy
		}
		if r.tasks != nil {
			for _, ancestor := range r.tasks.Ancestry(scope.key.TaskID) {
				if ancestor.ID == id {
					return task.ErrCompleteBusy
				}
			}
		}
	}
	return complete()
}
