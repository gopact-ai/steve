package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
)

// nativeImportProject adopts only the original directory from a node-validated
// import receipt. The config commit protocol serializes concurrent imports and
// preserves every existing workspace's ownership and access policy.
func (a *Service) nativeImportProject(ctx context.Context, name string, target config.Node, selected agent.Agent, workdir string) (string, error) {
	if a.Projects == nil {
		return "", errors.New(textFor(ctx).T(i18n.AdminNativeImportNeedsProjects))
	}
	if !filepath.IsAbs(workdir) || strings.ContainsRune(workdir, 0) {
		return "", errors.New(textFor(ctx).T(i18n.AdminNativeImportNoWorkdir))
	}
	workdir = filepath.Clean(workdir)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.ConfigStore.rlock()
	err := checkNativeImportTarget(textFor(ctx), a.cfg(), name, target, selected)
	a.ConfigStore.runlock()
	if err != nil {
		return "", err
	}
	found, err := a.inspect(ctx, name, workdir)
	if err != nil {
		return "", textFor(ctx).Errorf(i18n.AdminNativeImportWorkdirCheckFailed, err)
	}
	if len(found) == 1 && found[0].Missing {
		return "", errors.New(textFor(ctx).T(i18n.AdminNativeImportWorkdirGone))
	}
	var id string
	reused := errors.New("existing native import workspace")
	err = a.changeProjects(ctx, func(candidate *config.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.ConfigStore.rlock()
		err := checkNativeImportTarget(textFor(ctx), a.cfg(), name, target, selected)
		a.ConfigStore.runlock()
		if err != nil {
			return err
		}
		list, err := a.Projects.List(ctx)
		if err != nil {
			return err
		}
		for _, p := range list {
			for _, ws := range p.Workspaces() {
				if ws.Node != name || filepath.Clean(ws.Path) != workdir {
					continue
				}
				if id != "" && id != p.ID {
					return errors.New(textFor(ctx).T(i18n.AdminNativeImportManyProjects))
				}
				id = p.ID
			}
		}
		if id != "" {
			return reused
		}
		id = nativeImportProjectName(workdir)
		if _, exists := candidate.Projects[id]; exists || id == HomeProjectID {
			sum := sha256.Sum256([]byte(name + "\x00" + workdir))
			id += "-" + hex.EncodeToString(sum[:4])
		}
		if _, exists := candidate.Projects[id]; exists {
			return textFor(ctx).Errorf(i18n.AdminNativeImportProjectNameTaken, id)
		}
		candidate.Projects[id] = config.Project{Home: config.ProjectHome{Node: name, Path: workdir}, Level: string(datalevel.Internal), Repo: string(project.RepoInPlace)}
		return nil
	})
	if errors.Is(err, reused) {
		err = nil
	}
	return id, err
}

func checkNativeImportTarget(text i18n.Catalog, cfg *config.Config, name string, target config.Node, selected agent.Agent) error {
	if err := checkAgentNodeTarget(text, cfg, name, target); err != nil {
		return err
	}
	current, exists := cfg.Agents[selected.ID]
	if !exists || current.Node != name || current.Harness != selected.Harness {
		return errors.New(text.T(i18n.AdminAgentConfigChanged))
	}
	return nil
}

func nativeImportProjectName(workdir string) string {
	base := strings.ToLower(filepath.Base(workdir))
	var slug strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			slug.WriteRune(r)
		} else {
			slug.WriteByte('-')
		}
		if slug.Len() >= 32 {
			break
		}
	}
	prefix := strings.Trim(slug.String(), ".-_")
	if prefix == "" {
		prefix = "project"
	}
	return prefix
}
