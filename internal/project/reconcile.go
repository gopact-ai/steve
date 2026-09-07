package project

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const declarationKind = "project-declarations"
const declarationName = "operator-config"
const retiredKind = "retired-project"

var ErrDeclarationPending = errors.New("project configuration is committed but its projection is pending reconciliation")

type DeclarationState struct {
	Hash      string    `json:"hash"`
	AppliedAt time.Time `json:"applied_at"`
}

type retiredProject struct {
	Project   Project   `json:"project"`
	RetiredAt time.Time `json:"retired_at"`
}

// RequireDeclaration gates live resolution until this desired file revision is
// reflected by the ledger. Startup installs the desired revision before serving.
func (s *Store) RequireDeclaration(hash string) { s.declaration.Store(&hash) }

func (s *Store) AppliedDeclaration(ctx context.Context) (DeclarationState, error) {
	var state DeclarationState
	_, err := s.l.GetBinding(ctx, declarationKind, declarationName, &state)
	return state, err
}

func (s *Store) checkDeclaration(ctx context.Context) error {
	desired := s.declaration.Load()
	if desired == nil {
		return nil
	}
	state, err := s.AppliedDeclaration(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeclarationPending, err)
	}
	if state.Hash != *desired {
		return fmt.Errorf("%w: desired %s, applied %s", ErrDeclarationPending, *desired, state.Hash)
	}
	return nil
}

func (s *Store) normalizeDeclaration(ctx context.Context, desired []Project) (map[string]Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projects := make(map[string]Project, len(desired))
	for _, p := range desired {
		p.Copies = maps.Clone(p.Copies)
		p.ConfigGrants = maps.Clone(p.ConfigGrants)
		p.Skills, p.DurablePlaces = slices.Clone(p.Skills), slices.Clone(p.DurablePlaces)
		normalized, err := p.normalized()
		if err != nil {
			return nil, err
		}
		if _, exists := projects[normalized.ID]; exists {
			return nil, fmt.Errorf("project %s is declared more than once", normalized.ID)
		}
		for node, copy := range normalized.Copies {
			if copy.Origin != OriginAdopted && copy.Origin != OriginCloned {
				return nil, fmt.Errorf("project %s: unknown copy origin %q", p.ID, copy.Origin)
			}
			if copy.Origin == OriginCloned && copy.Source == "" {
				return nil, fmt.Errorf("project %s: cloned copy needs a source", p.ID)
			}
			if err := s.admits(normalized, node); err != nil {
				return nil, err
			}
		}
		projects[normalized.ID] = normalized
	}
	if err := validateOwnership(projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// ValidateDeclaration checks the complete final ownership set without changing
// either the configuration or its live project projection.
func (s *Store) ValidateDeclaration(ctx context.Context, desired []Project) error {
	next, err := s.normalizeDeclaration(ctx, desired)
	if err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error { return s.guardDeclaration(tx, next) })
}

// Reconcile replaces the active managed set and its revision in one transaction.
// Observed copy progress survives only while its declaration still names the
// same node, path and source. Removed project metadata remains available to
// historical readers, while Get/List/Materialize cannot use retired projects.
func (s *Store) Reconcile(ctx context.Context, desired []Project, hash string) error {
	next, err := s.normalizeDeclaration(ctx, desired)
	if err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		if err := s.guardDeclaration(tx, next); err != nil {
			return err
		}
		previous, err := projectsIn(tx)
		if err != nil {
			return err
		}
		for id, p := range next {
			for node, copy := range p.Copies {
				if old, exists := previous[id].Copies[node]; exists && sameCopyDeclaration(old, copy) {
					p.Copies[node] = old
				}
			}
			if err := tx.PutBinding(kindProject, id, p); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM bindings WHERE kind = ? AND id = ?", retiredKind, id); err != nil {
				return err
			}
		}
		for id, p := range previous {
			if _, keep := next[id]; keep {
				continue
			}
			if err := tx.PutBinding(retiredKind, id, retiredProject{Project: p, RetiredAt: s.now().UTC()}); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM bindings WHERE kind = ? AND id = ?", kindProject, id); err != nil {
				return err
			}
		}
		if err := s.reconcileConfigGrants(tx, next); err != nil {
			return err
		}
		return tx.PutBinding(declarationKind, declarationName, DeclarationState{Hash: hash, AppliedAt: s.now().UTC()})
	})
}

func sameCopyDeclaration(a, b Copy) bool {
	return a.Node == b.Node && a.Path == b.Path && a.Origin == b.Origin && a.Source == b.Source
}

// GetHistorical is explicitly for read-only artifact inspection. It bypasses
// the live declaration gate and may return metadata retained at retirement.
func (s *Store) GetHistorical(ctx context.Context, id string) (Project, bool, error) {
	var p Project
	if ok, err := s.l.GetBinding(ctx, kindProject, id, &p); ok || err != nil {
		return p, ok, err
	}
	var retired retiredProject
	ok, err := s.l.GetBinding(ctx, retiredKind, id, &retired)
	return retired.Project, ok, err
}

// UpdateDeclaredCopy records a clone outcome only for a still-declared copy.
// A late completion cannot recreate a removed project, copy or changed source.
func (s *Store) UpdateDeclaredCopy(ctx context.Context, projectID string, result Copy) error {
	if err := s.checkDeclaration(ctx); err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		all, err := projectsIn(tx)
		if err != nil {
			return err
		}
		p, exists := all[projectID]
		current, found := p.Copies[result.Node]
		if !exists || !found || !sameCopyDeclaration(current, result) {
			return fmt.Errorf("%w: copy declaration changed", ErrUnknown)
		}
		current.State, current.Error = result.State, result.Error
		if current.At.IsZero() {
			current.At, current.By = s.now().UTC(), result.By
		}
		p.Copies[result.Node] = current
		return tx.PutBinding(kindProject, projectID, p)
	})
}
