package plugins

import (
	"fmt"
	"slices"
)

// Relocation freezes portable package configuration and target-local secret
// references in the recovery plan, before the owner approves a replacement.
type Relocation struct {
	Selection   Selection    `json:"selection"`
	Deployments []Deployment `json:"deployments"`
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
