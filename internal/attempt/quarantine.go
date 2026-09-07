package attempt

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// Unsettled lists writers whose physical termination has not been verified.
// Terminal attempt state and lease expiry do not clear this quarantine.
func (s *Service) Unsettled(ctx context.Context) ([]Record, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return nil, err
		}
		if r.Unsettled {
			out = append(out, r)
		}
	}
	return out, nil
}

// CheckDeclarationsTx owns the attempt schema while project owns its
// declaration transaction. A still-unconfirmed writer cannot lose its directory
// assignment or have that physical path reused by another project.
func CheckDeclarationsTx(tx *ledger.Tx, desired []project.Project) error {
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return err
		}
		if !potentialWriter(r) || r.Workspace.Path == "" {
			continue
		}
		kept := r.Workspace.Kind == project.KindWorktree
		for _, p := range desired {
			places := []project.Home{p.Home}
			for node, copy := range p.Copies {
				places = append(places, project.Home{Node: node, Path: copy.Path})
			}
			for _, place := range places {
				if place.Node != r.Workspace.Node {
					continue
				}
				a, b := path.Clean(place.Path), path.Clean(r.Workspace.Path)
				same := p.ID == r.Project && a == b && r.Workspace.Kind != project.KindWorktree
				if same {
					kept = true
					continue
				}
				if a == b || strings.HasPrefix(a, strings.TrimRight(b, "/")+"/") || strings.HasPrefix(b, strings.TrimRight(a, "/")+"/") {
					return fmt.Errorf("attempt %s has an unconfirmed writer at %s; directory ownership cannot change", r.ID, r.Workspace.Path)
				}
			}
		}
		if !kept {
			return fmt.Errorf("attempt %s has an unconfirmed writer; its workspace cannot be retired or moved", r.ID)
		}
	}
	return nil
}
