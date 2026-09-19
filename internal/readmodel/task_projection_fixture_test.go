package readmodel

import (
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

// Legacy projection scenarios explicitly supply the entire fixture. Production
// uses owner summaries instead of recomputing eligibility on a partial workset.
func tasks(list []task.Task, plans map[string]plan.Plan) []Task {
	completable := task.CompletionEligibility(list)
	children := map[string]int{}
	for _, t := range list {
		children[t.Parent]++
	}
	headers := make([]task.Header, 0, len(list))
	for _, t := range list {
		headers = append(headers, task.Header{Task: t, Summary: task.ReadSummary{CanComplete: completable[t.ID], Children: children[t.ID]}})
	}
	return projectTasks(headers, plans)
}
