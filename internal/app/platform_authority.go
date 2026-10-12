package app

import (
	"context"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/control"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

// Platform consumers depend on the control policy, not the native executor.
// Both the original MCP grant and caller policy share the consumer's commit.
func configurePlatformAuthority(gate *agentmcp.Server, book *ledger.Ledger, accounts *identity.Accounts, admin *adminsvc.Service, view *readmodel.Model) {
	policy := control.ForAccounts(accounts)
	check := func(ctx context.Context, tx *ledger.Tx, bind agentmcp.Binding, change bool) error {
		if err := agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx}); err != nil {
			return err
		}
		scope, ok := agentmcp.ScopeFromContext(ctx)
		if !ok {
			return agentmcp.ErrGrantDenied
		}
		return policy.ExecutionAccessTx(tx, scope.AttemptID, bind.ConversationID, change, bind.DelegatedBy != "" || bind.TaskID != "")
	}
	gate.SetPlatformAuthority(func(ctx context.Context, bind agentmcp.Binding, change bool) error {
		return book.Update(ctx, func(tx *ledger.Tx) error { return check(ctx, tx, bind, change) })
	})
	if admin != nil {
		admin.ConfigCommit = standaloneConfigurationCommit(book)
	}
	gate.SetFleeter(adminsvc.FleetTools{Admin: admin, View: view, Guard: func(ctx context.Context, tx *ledger.Tx) error {
		scope, ok := agentmcp.ScopeFromContext(ctx)
		if !ok {
			return agentmcp.ErrGrantDenied
		}
		tracked, found, err := task.GetTx(tx, scope.TaskID)
		if err != nil {
			return err
		}
		if !found {
			return agentmcp.ErrGrantDenied
		}
		return check(ctx, tx, agentmcp.Binding{ConversationID: tracked.Channel, AgentID: tracked.Member, DelegatedBy: tracked.Parent}, true)
	}})
}

func standaloneConfigurationCommit(book *ledger.Ledger) func(context.Context, func() error) error {
	return func(ctx context.Context, save func() error) error {
		guard := control.MutationGuard(ctx)
		if guard == nil {
			return save()
		}
		return book.Update(ctx, func(tx *ledger.Tx) error {
			if err := guard(tx); err != nil {
				return err
			}
			return save()
		})
	}
}
