package readmodel

import (
	"context"
	"time"
)

// UsageSnapshot is an independent, on-demand read of closed execution history.
// A nil Usage means no data was readable, not that the measured usage was zero.
// Partial results retain their source error and must not be presented as complete.
type UsageSnapshot struct {
	At      time.Time      `json:"at"`
	Usage   *Usage         `json:"usage,omitempty"`
	Sources []SourceHealth `json:"sources"`
}

// UsageSummary reads only settled attempts' usage samples and the task
// ownership metadata used by usage. It neither builds the fleet snapshot nor
// caches the summary across reads.
func (m *Model) UsageSummary(ctx context.Context) UsageSnapshot {
	result := UsageSnapshot{At: time.Now(), Sources: []SourceHealth{{Name: "ledger-usage", Wired: m.src.Ledger != nil}}}
	if m.src.Ledger == nil {
		return result
	}
	closed, err := m.src.Ledger.UsageSamples(ctx)
	if err != nil {
		result.Sources[0].Error = err.Error()
		if len(closed) == 0 {
			return result
		}
	}
	var roots []Task
	if m.src.Tasks != nil {
		list := m.src.Tasks.List("")
		roots = make([]Task, 0, len(list))
		for _, item := range list {
			meta := m.src.Tasks.MetaOf(item.ID)
			roots = append(roots, Task{ID: item.ID, Parent: item.Parent, Goal: item.Goal, Title: meta.Title, Origin: item.Origin, ProjectID: item.ProjectID})
		}
	}
	summary := usage(closed, result.At, roots)
	result.Usage = &summary
	return result
}
