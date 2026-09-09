package plugins

import (
	"context"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

const installationHistoryKind = "plugin-installation-history"

// RememberTargets runs before changing a declaration. A node removed from
// current scope may still hold a process whose open response was lost.
func (l *Library) RememberTargets(ctx context.Context, id string, item Installation) error {
	return l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings(installationHistoryKind)
		if err != nil {
			return err
		}
		var nodes []string
		if raw, ok := rows[id]; ok {
			if err := decodeStrict(raw, &nodes); err != nil {
				return err
			}
		}
		for node := range item.Targets {
			if !slices.Contains(nodes, node) {
				nodes = append(nodes, node)
			}
		}
		slices.Sort(nodes)
		return tx.PutBinding(installationHistoryKind, id, nodes)
	})
}

func (l *Library) InstallationNodes(ctx context.Context, id string) ([]string, error) {
	var nodes []string
	_, err := l.Ledger.GetBinding(ctx, installationHistoryKind, id, &nodes)
	if err != nil {
		return nil, err
	}
	rows, err := l.Ledger.Bindings(ctx, deploymentRecordKind)
	if err != nil {
		return nil, err
	}
	for _, raw := range rows {
		var receipt DeploymentReceipt
		if err := decodeStrict(raw, &receipt); err != nil {
			return nil, err
		}
		if receipt.Deployment.Installation == id && !slices.Contains(nodes, receipt.Deployment.Node) {
			nodes = append(nodes, receipt.Deployment.Node)
		}
	}
	slices.Sort(nodes)
	return nodes, nil
}
