package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/gopact-ai/steve/internal/project"
)

// ProjectController applies the config-owned project declarations to the
// ledger. The file is the durable recovery intent; only the applied hash lives
// with the projection, so a failed second write never creates two authorities.
type ProjectController struct{ Store *project.Store }

type ProjectionPendingError struct {
	Hash string
	Err  error
}

func (e *ProjectionPendingError) Error() string {
	return "配置已保存，项目投影尚未应用；请重试或重启恢复：" + e.Err.Error()
}
func (e *ProjectionPendingError) Unwrap() error { return e.Err }

func ProjectDeclarations(c *Config) ([]project.Project, string, error) {
	if c.Gateway.DefaultProject != "" {
		if _, ok := c.Projects[c.Gateway.DefaultProject]; !ok {
			return nil, "", fmt.Errorf("default project %s is not declared", c.Gateway.DefaultProject)
		}
	}
	for id, p := range c.Projects {
		if id == ReservedHomeProject {
			return nil, "", fmt.Errorf("project %s is reserved", id)
		}
		if p.Home.Node != "" {
			if _, ok := c.Nodes[p.Home.Node]; !ok {
				return nil, "", fmt.Errorf("project %s names unknown home node %s", id, p.Home.Node)
			}
		}
		seen := map[string]bool{}
		for _, w := range p.Workspaces {
			if seen[w.Node] {
				return nil, "", fmt.Errorf("project %s declares multiple copies on %s", id, w.Node)
			}
			seen[w.Node] = true
			if w.Origin != "" && w.Origin != string(project.OriginAdopted) && w.Origin != string(project.OriginCloned) {
				return nil, "", fmt.Errorf("project %s has unknown workspace origin %q", id, w.Origin)
			}
			if w.Source != "" && w.Origin != string(project.OriginCloned) {
				return nil, "", fmt.Errorf("project %s: only a cloned workspace has a source", id)
			}
			if w.Node != "" {
				if _, ok := c.Nodes[w.Node]; !ok {
					return nil, "", fmt.Errorf("project %s names unknown copy node %s", id, w.Node)
				}
			}
			level := c.HubLevel()
			if w.Node != "" {
				level = project.Level(c.Nodes[w.Node].Level).OrDefault()
			}
			if !project.Level(p.Level).OrDefault().Admits(level) {
				return nil, "", fmt.Errorf("project %s cannot place its data on %s", id, w.Node)
			}
		}
	}
	desired := c.ProjectList()
	if c.RuntimeHome != nil {
		desired = append(desired, project.Project{ID: ReservedHomeProject, Level: project.LevelRestricted, Home: project.Home{Node: c.RuntimeHome.Node, Path: c.RuntimeHome.Path}})
	} else if c.Gateway.HomePath != "" {
		desired = append(desired, project.Project{ID: ReservedHomeProject, Level: project.LevelRestricted, Home: project.Home{Path: c.Gateway.HomePath}})
	}
	raw, err := json.Marshal(struct {
		Projects []project.Project
		Default  string
	}{desired, c.Gateway.DefaultProject})
	return desired, fingerprint(raw), err
}

// CloneProjects isolates candidate declaration maps/slices from the live config.
func CloneProjects(c *Config) *Config {
	next := *c
	next.Projects = maps.Clone(c.Projects)
	if next.Projects == nil {
		next.Projects = map[string]Project{}
	}
	for id, p := range next.Projects {
		p.Skills, p.DurablePlaces = slices.Clone(p.Skills), slices.Clone(p.DurablePlaces)
		p.Workspaces, p.Grants = slices.Clone(p.Workspaces), maps.Clone(p.Grants)
		next.Projects[id] = p
	}
	return &next
}

// Reconcile is shared by startup and explicit recovery. It always rebuilds the
// complete active set from the current file-owned declaration, including removals.
func (c ProjectController) Reconcile(ctx context.Context, cfg *Config) error {
	desired, hash, err := ProjectDeclarations(cfg)
	if err != nil {
		return err
	}
	c.Store.RequireDeclaration(hash)
	if err := c.Store.Reconcile(ctx, desired, hash); err != nil {
		return &ProjectionPendingError{Hash: hash, Err: err}
	}
	return nil
}

func (c ProjectController) Ensure(ctx context.Context, cfg *Config) error {
	_, hash, err := ProjectDeclarations(cfg)
	if err != nil {
		return err
	}
	state, err := c.Store.AppliedDeclaration(ctx)
	if err != nil {
		return err
	}
	c.Store.RequireDeclaration(hash)
	if state.Hash == hash {
		return nil
	}
	return c.Reconcile(ctx, cfg)
}

// Commit validates before committing the operator file. commit must publish
// the same candidate to the live config after a successful/committed save.
func (c ProjectController) Commit(ctx context.Context, candidate *Config, commit func() error) error {
	if len(candidate.Projects) == 0 {
		return errors.New("至少保留一个配置项目；配置文件不接受空 projects")
	}
	desired, hash, err := ProjectDeclarations(candidate)
	if err != nil {
		return err
	}
	if err := c.Store.ValidateDeclaration(ctx, desired); err != nil {
		return err
	}
	previous, err := c.Store.AppliedDeclaration(ctx)
	if err != nil {
		return err
	}
	// Close live resolution before the file commit boundary; a failed save
	// restores the applied revision, while a committed failure stays gated.
	c.Store.RequireDeclaration(hash)
	saveErr := commit()
	if saveErr != nil && !Committed(saveErr) {
		c.Store.RequireDeclaration(previous.Hash)
		return saveErr
	}
	if err := c.Store.Reconcile(ctx, desired, hash); err != nil {
		return errors.Join(saveErr, &ProjectionPendingError{Hash: hash, Err: err})
	}
	return saveErr
}
