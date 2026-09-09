package plugins

import (
	"context"
	"fmt"
	"slices"
)

const deploymentRecordKind = "plugin-deployment"

func (l *Library) RecordDeployment(ctx context.Context, receipt DeploymentReceipt) error {
	hash, err := receipt.Deployment.Hash()
	if err != nil || receipt.Schema != Schema || receipt.Hash != hash || receipt.PreparedAt.IsZero() {
		return ErrIntegrity
	}
	return l.Ledger.PutBinding(ctx, deploymentRecordKind, hash, receipt)
}

func (l *Library) Deployment(ctx context.Context, hash string) (DeploymentReceipt, error) {
	var receipt DeploymentReceipt
	if !digestShape.MatchString(hash) {
		return receipt, ErrInvalid
	}
	found, err := l.Ledger.GetBinding(ctx, deploymentRecordKind, hash, &receipt)
	if err != nil {
		return receipt, err
	}
	if !found {
		return receipt, ErrUnavailable
	}
	actual, err := receipt.Deployment.Hash()
	if err != nil || actual != hash || receipt.Hash != hash || receipt.Schema != Schema {
		return receipt, ErrIntegrity
	}
	return receipt, nil
}

// CheckRuntimeScope preserves old content versions while consulting current
// authority. Disabling new bindings is distinct from revoking project scope.
func (l *Library) CheckRuntimeScope(ctx context.Context, items map[string]Installation, ref RuntimeRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	for _, hash := range ref.Selection.Deployments {
		receipt, err := l.Deployment(ctx, hash)
		if err != nil {
			return err
		}
		deployed := receipt.Deployment
		item, ok := items[deployed.Installation]
		if !ok || item.PackageID != deployed.PackageID || deployed.Node != ref.Selection.Node || !slices.Contains(deployed.Projects, ref.Selection.Project) || !slices.Contains(item.Projects, ref.Selection.Project) {
			return fmt.Errorf("%w: plugin project authorization was revoked", ErrUnavailable)
		}
		if _, ok := item.Targets[ref.Selection.Node]; !ok {
			return fmt.Errorf("%w: plugin node authorization was revoked", ErrUnavailable)
		}
	}
	return nil
}
