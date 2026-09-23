package pluginledger

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/gopact-ai/steve/internal/plugins"
)

const deploymentRecordKind = "plugin-deployment"

func (l *Library) RecordDeployment(ctx context.Context, receipt plugins.DeploymentReceipt) error {
	hash, err := receipt.Deployment.Hash()
	if err != nil || receipt.Schema != plugins.Schema || receipt.Hash != hash || receipt.PreparedAt.IsZero() {
		return plugins.ErrIntegrity
	}
	return l.Ledger.PutBinding(ctx, deploymentRecordKind, hash, receipt)
}

func (l *Library) Deployment(ctx context.Context, hash string) (plugins.DeploymentReceipt, error) {
	var receipt plugins.DeploymentReceipt
	if !plugins.ValidDigest(hash) {
		return receipt, plugins.ErrInvalid
	}
	found, err := l.Ledger.GetBinding(ctx, deploymentRecordKind, hash, &receipt)
	if err != nil {
		return receipt, err
	}
	if !found {
		return receipt, plugins.ErrUnavailable
	}
	actual, err := receipt.Deployment.Hash()
	if err != nil || actual != hash || receipt.Hash != hash || receipt.Schema != plugins.Schema {
		return receipt, plugins.ErrIntegrity
	}
	return receipt, nil
}

// CheckRuntimeScope preserves old content versions while consulting current
// authority. Disabling new bindings is distinct from revoking project scope.
func (l *Library) CheckRuntimeScope(ctx context.Context, items map[string]plugins.Installation, ref plugins.RuntimeRef) error {
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
			return fmt.Errorf("%w: plugin project authorization was revoked", plugins.ErrUnavailable)
		}
		if _, ok := item.Targets[ref.Selection.Node]; !ok {
			return fmt.Errorf("%w: plugin node authorization was revoked", plugins.ErrUnavailable)
		}
	}
	return nil
}

func (l *Library) PlanRelocation(ctx context.Context, items map[string]plugins.Installation, source plugins.RuntimeRef, node, harness string) (*plugins.Relocation, error) {
	if err := l.CheckRuntimeScope(ctx, items, source); err != nil {
		return nil, err
	}
	plan := &plugins.Relocation{Selection: source.Selection.Clone()}
	plan.Selection.Node, plan.Selection.Harness, plan.Selection.Deployments = node, harness, nil
	for _, hash := range source.Selection.Deployments {
		previous, err := l.Deployment(ctx, hash)
		if err != nil {
			return nil, err
		}
		d := previous.Deployment
		item := items[d.Installation]
		target, ok := item.Targets[node]
		if !ok || !slices.Contains(item.Projects, source.Selection.Project) {
			return nil, fmt.Errorf("%w: recovery target is outside plugin scope", plugins.ErrUnavailable)
		}
		// Node credentials cannot be copied from the failed machine. The
		// owner's current target references are fixed alongside the old code.
		d.Node = node
		d.Projects = []string{source.Selection.Project}
		d.Configuration = d.Configuration.Clone()
		d.Configuration.Secrets = maps.Clone(target.Secrets)
		bundle, err := l.Get(ctx, source.Selection.Project, d.Digest)
		if err != nil {
			return nil, err
		}
		if err := bundle.Manifest.CheckConfiguration(d.Configuration); err != nil {
			return nil, err
		}
		hash, err := d.Hash()
		if err != nil {
			return nil, err
		}
		plan.Deployments = append(plan.Deployments, d)
		plan.Selection.Deployments = append(plan.Selection.Deployments, hash)
	}
	_, err := plan.Selection.Hash()
	return plan, err
}
