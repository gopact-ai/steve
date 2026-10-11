package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// AuthorizePlatform is the consumer-side policy for platform MCP operations.
// The MCP store independently validates the exact original grant, native
// session, attempt, activation and leases. This adds resource/owner policy.
func (c *Coordinator) AuthorizePlatform(ctx context.Context, bind agentmcp.Binding, change bool) error {
	accounts := c.owners.Configuration().Accounts
	if accounts == nil {
		return nil
	} // Explicit trusted in-process test/embedding mode.
	return accounts.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		return c.AuthorizePlatformTx(ctx, tx, bind, change)
	})
}

// AuthorizePlatformTx shares the caller's actual configuration commit.
func (c *Coordinator) AuthorizePlatformTx(ctx context.Context, tx ledger.Reader, bind agentmcp.Binding, change bool) error {
	accounts := c.owners.Configuration().Accounts
	if accounts == nil {
		return nil
	}
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		return agentmcp.ErrGrantDenied
	}
	proof, tracked, err := c.controls.ExecutionCallerTx(tx, scope.AttemptID)
	if err != nil {
		return err
	}
	if tracked.Channel != bind.ConversationID {
		return agentmcp.ErrGrantDenied
	}
	if change {
		return c.controls.ExecutionOwnerTx(tx, scope.AttemptID, bind.DelegatedBy != "" || bind.TaskID != "")
	}
	role, err := project.AccessPolicyTx(tx, tracked.ProjectID, proof.Principal, accounts.Owner)
	if err != nil {
		return err
	}
	if !role.AtLeast(project.RoleRead) {
		return agentmcp.ErrGrantDenied
	}
	return nil
}
