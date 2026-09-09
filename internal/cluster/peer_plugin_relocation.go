package cluster

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/plugins"
)

func authorizePluginRelocation(ctx context.Context, book *ledger.Ledger, declaration platformconfig.Declaration, req nodewire.PluginRequest) error {
	service := attempt.New(book)
	plan, err := service.Relocation(ctx, req.RelocationPlan)
	if err != nil {
		return err
	}
	if plan.Plugins == nil || plan.Target.Node != req.Node || plan.Plugins.Selection.Node != req.Node || plan.Plugins.Selection.Project != plan.Target.Project || plan.Plugins.Selection.Harness != plan.Target.Harness {
		return plugins.ErrInvalid
	}
	record, err := service.Get(ctx, plan.Target.ID)
	if err != nil {
		return err
	}
	if !attempt.PreparingRelocation(record) || record.Recovery == nil || record.Recovery.PlanID != plan.ID {
		return errors.New("plugin recovery requires an admitted relocation")
	}
	if err := plan.Plugins.CheckScope(declaration.Plugins); err != nil {
		return err
	}
	switch req.Action {
	case nodewire.PluginPrepare:
		got, err := req.Deployment.Hash()
		if err != nil {
			return err
		}
		for _, d := range plan.Plugins.Deployments {
			wanted, err := d.Hash()
			if err != nil {
				return err
			}
			if got == wanted {
				return nil
			}
		}
	case nodewire.PluginRuntimePrepare:
		if req.Selection == nil {
			return plugins.ErrInvalid
		}
		want, err := plan.Plugins.Selection.Hash()
		if err != nil {
			return err
		}
		actual, err := req.Selection.Hash()
		if err == nil && actual == want {
			return nil
		}
	}
	return plugins.ErrConflict
}
