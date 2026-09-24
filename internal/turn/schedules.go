package turn

import (
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

// Schedules manages standing work on behalf of an agent's fixed execution.
// It reads only durable state, so it runs apart from any Coordinator.
type Schedules struct {
	attempts  *attempt.Service
	tasks     *task.Store
	projects  *project.Store
	schedules *schedule.Store
	text      i18n.Catalog
	owners    channelOwners
}

// ScheduleDeps is everything Schedules is built with. NewSchedules refuses
// one missing anything ScheduleDeps.required lists.
type ScheduleDeps struct {
	Attempts  *attempt.Service
	Tasks     *task.Store
	Projects  *project.Store
	Schedules *schedule.Store
	Text      i18n.Catalog
	// Owner is the baseline owner identity; ChannelOwners registers each
	// trusted non-console adapter's native owner.
	Owner         string
	ChannelOwners map[string]string
}

func (d ScheduleDeps) required() []dependency {
	return []dependency{
		{"Attempts", d.Attempts == nil}, {"Tasks", d.Tasks == nil}, {"Projects", d.Projects == nil},
		{"Schedules", d.Schedules == nil}, {"Text", d.Text.IsZero()},
	}
}

// NewSchedules builds Schedules from deps, or reports every dependency missing.
func NewSchedules(deps ScheduleDeps) (*Schedules, error) {
	if missing := absent(deps.required()); len(missing) > 0 {
		return nil, fmt.Errorf("turn: missing schedule dependencies: %s", strings.Join(missing, ", "))
	}
	owners, err := newChannelOwners(deps.Owner, deps.ChannelOwners)
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	return &Schedules{
		attempts: deps.Attempts, tasks: deps.Tasks, projects: deps.Projects,
		schedules: deps.Schedules, text: deps.Text, owners: owners,
	}, nil
}
