package admin

import (
	"context"
	"errors"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// ProjectDirShape is one segment of a project's directory: a plain name,
// nothing that walks out of the workspace or hides.
var ProjectDirShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckProjectDir accepts the directory a project is given. It is always
// relative to the machine's workspace, never a path of its own: the same
// project must be the same relative directory on every machine, or a copy
// on a second machine lands somewhere the first has never heard of.
func CheckProjectDir(text i18n.Catalog, input string) (string, error) {
	dir := strings.TrimSpace(input)
	if dir == "" {
		return "", errors.New(text.T(i18n.AdminProjectDirRelative))
	}
	if strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~") || strings.Contains(dir, "\\") {
		return "", text.Errorf(i18n.AdminProjectDirAbsolute, input)
	}
	dir = strings.Trim(dir, "/")
	for _, segment := range strings.Split(dir, "/") {
		if !ProjectDirShape.MatchString(segment) {
			return "", text.Errorf(i18n.AdminProjectDirInvalid, input)
		}
	}
	return dir, nil
}

// localMachine reports whether the page named this machine: outside a
// cluster the hub is the empty name, inside it the node's own identity.
func (a *Service) localMachine(nodeKey string) bool {
	return nodeKey == "" || a.ClusterMode && nodeKey == a.NodeName
}

// workspaceRootOf is where a machine keeps its projects: what it says
// about itself, which for an enrolled machine is the directory it was
// enrolled with. This machine falls back to its own derivation while its
// worker is still starting; every other machine must answer.
func (a *Service) workspaceRootOf(ctx context.Context, nodeKey string) (string, error) {
	local := a.localMachine(nodeKey)
	if a.Nodes != nil && nodeKey != "" {
		actx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		advert, err := a.Nodes.Advert(actx, nodeKey)
		switch {
		case err != nil && !local:
			return "", textFor(ctx).Errorf(i18n.AdminWorkspaceRootReadFailed, a.name(nodeKey), err)
		case err == nil && advert.WorkspaceRoot != "":
			return advert.WorkspaceRoot, nil
		case !local:
			return "", textFor(ctx).Errorf(i18n.AdminWorkspaceRootUnreported, a.name(nodeKey))
		}
	}
	if !local {
		return "", textFor(ctx).Errorf(i18n.AdminWorkspaceRootUnknown, a.name(nodeKey))
	}
	a.ConfigStore.rlock()
	defer a.ConfigStore.runlock()
	return a.cfg().LocalWorkspaceRoot(), nil
}

// projectPath resolves a project's directory on one machine.
func (a *Service) projectPath(ctx context.Context, nodeKey, dir string) (string, error) {
	clean, err := CheckProjectDir(textFor(ctx), dir)
	if err != nil {
		return "", err
	}
	root, err := a.workspaceRootOf(ctx, nodeKey)
	if err != nil {
		return "", err
	}
	return path.Join(nodewire.ProjectsDir(root), clean), nil
}

// projectDirName is the directory a project is known by, relative to a
// machine's projects root. A copy on another machine reuses it, so the
// same project is the same relative directory everywhere and an agent
// that moves still finds its work. A home that predates this rule, or the
// workspace handed over whole by the desktop guide, falls back to the
// project's own name.
func (a *Service) projectDirName(ctx context.Context, projectID string) (string, error) {
	a.ConfigStore.rlock()
	item, exists := a.cfg().Projects[projectID]
	a.ConfigStore.runlock()
	if !exists {
		return "", textFor(ctx).Errorf(i18n.AdminNoProject, projectID)
	}
	root, err := a.workspaceRootOf(ctx, item.Home.Node)
	if err != nil {
		// A home on a machine that cannot answer right now still gets a
		// copy: its directory falls back to the project name, which is
		// what an unnamed directory has always resolved to.
		return projectID, nil
	}
	prefix := strings.TrimSuffix(nodewire.ProjectsDir(root), "/") + "/"
	rel := strings.TrimPrefix(item.Home.Path, prefix)
	if rel == item.Home.Path || rel == "" {
		return projectID, nil
	}
	return rel, nil
}

// makeProjectDir creates the directory a project was given, on whichever
// machine holds it. A directory that is not there yet is the normal case:
// the owner names a project, the machine makes room for it.
func (a *Service) makeProjectDir(ctx context.Context, nodeKey, dir string) error {
	if a.localMachine(nodeKey) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return textFor(ctx).Errorf(i18n.AdminMakeDirFailed, dir, err)
		}
		return nil
	}
	if a.Nodes == nil {
		return textFor(ctx).Errorf(i18n.AdminCannotMakeDirYet, a.name(nodeKey))
	}
	if _, err := a.Nodes.Files(ctx, nodeKey, nodewire.FileRequest{Op: nodewire.FileMkdir, Path: dir}); err != nil {
		return textFor(ctx).Errorf(i18n.AdminMakeDirOnFailed, a.name(nodeKey), dir, err)
	}
	return nil
}
