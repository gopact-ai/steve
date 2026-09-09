package plugins

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// Relocation freezes portable package configuration and target-local secret
// references in the recovery plan, before the owner approves a replacement.
type Relocation struct {
	Selection   Selection    `json:"selection"`
	Deployments []Deployment `json:"deployments"`
}

func (l *Library) PlanRelocation(ctx context.Context, items map[string]Installation, source RuntimeRef, node, harness string) (*Relocation, error) {
	if err := l.CheckRuntimeScope(ctx, items, source); err != nil {
		return nil, err
	}
	plan := &Relocation{Selection: source.Selection.Clone()}
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
			return nil, fmt.Errorf("%w: recovery target is outside plugin scope", ErrUnavailable)
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

func (p Relocation) CheckScope(items map[string]Installation) error {
	if _, err := p.Selection.Hash(); err != nil {
		return err
	}
	if len(p.Selection.Deployments) != len(p.Deployments) {
		return ErrIntegrity
	}
	for _, d := range p.Deployments {
		hash, err := d.Hash()
		if err != nil || !slices.Contains(p.Selection.Deployments, hash) || d.Node != p.Selection.Node || !slices.Contains(d.Projects, p.Selection.Project) {
			return ErrIntegrity
		}
		item, ok := items[d.Installation]
		if !ok || item.PackageID != d.PackageID || !slices.Contains(item.Projects, p.Selection.Project) {
			return fmt.Errorf("%w: recovery project scope was revoked", ErrUnavailable)
		}
		if _, ok := item.Targets[p.Selection.Node]; !ok {
			return fmt.Errorf("%w: recovery node scope was revoked", ErrUnavailable)
		}
	}
	return nil
}
