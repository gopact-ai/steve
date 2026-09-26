package configbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
)

// ProjectList renders projects{} as the runtime's records.
func ProjectList(c *config.Config) []project.Project {
	out := make([]project.Project, 0, len(c.Projects))
	for id, item := range c.Projects {
		p := project.Project{
			ID: id, Level: datalevel.Level(item.Level), Repo: project.RepoMode(item.Repo), Skills: item.Skills,
			DurablePlaces: item.DurablePlaces, ExternalRemote: item.ExternalRemote, DefaultRole: project.Role(item.DefaultRole),
			Home: project.Home{Node: item.Home.Node, Path: item.Home.Path},
		}
		if len(item.Grants) > 0 {
			p.ConfigGrants = make(map[string]project.Role, len(item.Grants))
			for principal, role := range item.Grants {
				p.ConfigGrants[principal] = project.Role(role)
			}
		}
		for _, ws := range item.Workspaces {
			if p.Copies == nil {
				p.Copies = map[string]project.Copy{}
			}
			copy := project.Copy{Node: ws.Node, Path: ws.Path, Origin: project.Origin(ws.Origin), Source: ws.Source}
			if copy.Origin == project.OriginCloned {
				copy.State = project.CopyProvisioning
			}
			p.Copies[ws.Node] = copy
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GrantList renders every configured grant.
func GrantList(c *config.Config) []project.Grant {
	var out []project.Grant
	for id, item := range c.Projects {
		for principal, role := range item.Grants {
			out = append(out, project.Grant{Project: id, Principal: principal, Role: project.Role(role), By: "config"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Principal < out[j].Principal
	})
	return out
}

// ProjectController applies the config-owned project declarations to the
// ledger. The file is the durable recovery intent; only the applied hash lives
// with the projection, so a failed second write never creates two authorities.
type ProjectController struct{ Store *project.Store }

// ProjectionPendingError says in the language of the change's author that
// the file is saved but its projects are not applied yet.
type ProjectionPendingError struct {
	Hash string
	Err  error
	text i18n.Catalog
}

func (e *ProjectionPendingError) Error() string {
	return e.text.T(i18n.ConfigProjectionPending, e.Err)
}
func (e *ProjectionPendingError) Unwrap() error { return e.Err }

func ProjectDeclarations(c *config.Config) ([]project.Project, string, error) {
	if c.Gateway.DefaultProject != "" {
		if _, ok := c.Projects[c.Gateway.DefaultProject]; !ok {
			return nil, "", fmt.Errorf("default project %s is not declared", c.Gateway.DefaultProject)
		}
	}
	for id, p := range c.Projects {
		if id == config.ReservedHomeProject {
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
				level = datalevel.Level(c.Nodes[w.Node].Level).OrDefault()
			}
			if !datalevel.Level(p.Level).OrDefault().Admits(level) {
				return nil, "", fmt.Errorf("project %s cannot place its data on %s", id, w.Node)
			}
		}
	}
	desired := ProjectList(c)
	if c.RuntimeHome != nil {
		desired = append(desired, project.Project{ID: config.ReservedHomeProject, Level: datalevel.Restricted, Home: project.Home{Node: c.RuntimeHome.Node, Path: c.RuntimeHome.Path}})
	} else if c.Gateway.HomePath != "" {
		desired = append(desired, project.Project{ID: config.ReservedHomeProject, Level: datalevel.Restricted, Home: project.Home{Path: c.Gateway.HomePath}})
	}
	raw, err := json.Marshal(struct {
		Projects []project.Project
		Default  string
	}{desired, c.Gateway.DefaultProject})
	return desired, declarationHash(raw), err
}

// Reconcile is shared by startup and explicit recovery. It always rebuilds the
// complete active set from the current file-owned declaration, including removals.
func (c ProjectController) Reconcile(ctx context.Context, cfg *config.Config) error {
	desired, hash, err := ProjectDeclarations(cfg)
	if err != nil {
		return err
	}
	c.Store.RequireDeclaration(hash)
	if err := c.Store.Reconcile(ctx, desired, hash); err != nil {
		return &ProjectionPendingError{Hash: hash, Err: err, text: i18n.FromContext(ctx)}
	}
	return nil
}

func (c ProjectController) Ensure(ctx context.Context, cfg *config.Config) error {
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
func (c ProjectController) Commit(ctx context.Context, candidate *config.Config, commit func() error) error {
	if len(candidate.Projects) == 0 {
		return errors.New(i18n.FromContext(ctx).T(i18n.ConfigProjectsRequired))
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
	if saveErr != nil && !config.Committed(saveErr) {
		c.Store.RequireDeclaration(previous.Hash)
		return saveErr
	}
	if err := c.Store.Reconcile(ctx, desired, hash); err != nil {
		return errors.Join(saveErr, &ProjectionPendingError{Hash: hash, Err: err, text: i18n.FromContext(ctx)})
	}
	return saveErr
}

func declarationHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
