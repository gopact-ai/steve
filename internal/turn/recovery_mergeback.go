package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// WorkspaceRecoveryControl authorizes one bounded recovery driver step under
// the same owner/maintenance serialization as the abandonment decision.
type WorkspaceRecoveryControl struct{ coordinator *Coordinator }

func NewWorkspaceRecoveryControl(c *Coordinator) *WorkspaceRecoveryControl {
	return &WorkspaceRecoveryControl{coordinator: c}
}

func (a *WorkspaceRecoveryControl) Drive(ctx context.Context, id string, step func(context.Context, ledger.Lease) error) error {
	c := a.coordinator
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	owner, err := c.owners.of("console")
	if err != nil {
		return err
	}
	r, err := c.attempts.WorkspaceRecovery(ctx, id)
	if err != nil {
		return err
	}
	if c.maintaining || owner == "" || owner != r.RequestedBy {
		return errors.New("workspace recovery awaits its current owner outside maintenance")
	}
	lifetime := c.executions.Detached(ctx)
	lifetime, end := context.WithTimeout(lifetime, 5*time.Minute)
	defer end()
	return c.attempts.DriveWorkspaceRecovery(ctx, lifetime, id, step)
}

// Replay consumes only a durable applying/committed decision, not fresh work.
// A later owner or maintenance change cannot turn it into new authorization.
func (a *WorkspaceRecoveryControl) Replay(ctx context.Context, id string) (bool, error) {
	c := a.coordinator
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	return c.artifacts.ReplayAdmittedRecovery(ctx, id)
}
