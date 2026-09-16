package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// ProjectDirShape is one segment of a project's directory: a plain name,
// nothing that walks out of the workspace or hides.
var ProjectDirShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckProjectDir accepts the directory a project is given. It is always
// relative to the machine's workspace, never a path of its own: the same
// project must be the same relative directory on every machine, or a copy
// on a second machine lands somewhere the first has never heard of.
func CheckProjectDir(input string) (string, error) {
	dir := strings.TrimSpace(input)
	if dir == "" {
		return "", errors.New("目录要写工作区下的相对路径，例如 my-service")
	}
	if strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~") || strings.Contains(dir, "\\") {
		return "", fmt.Errorf("目录 %q 不能是绝对路径：只写工作区下的相对路径，每台机器各自拼上自己的工作区", input)
	}
	dir = strings.Trim(dir, "/")
	for _, segment := range strings.Split(dir, "/") {
		if !ProjectDirShape.MatchString(segment) {
			return "", fmt.Errorf("目录 %q 不能用：每一段只能是字母、数字、点、下划线、连字符，且不能以点开头", input)
		}
	}
	return dir, nil
}

// localMachine reports whether the page named this machine: outside a
// cluster the hub is the empty name, inside it the node's own identity.
func (a *Service) localMachine(nodeKey string) bool {
	return nodeKey == "" || a.ClusterMode && nodeKey == NodeName()
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
			return "", fmt.Errorf("读取 %s 的工作区目录失败：%w", nodewire.Place(nodeKey), err)
		case err == nil && advert.WorkspaceRoot != "":
			return advert.WorkspaceRoot, nil
		case !local:
			return "", fmt.Errorf("%s 还没有报告自己的工作区目录，等它连上再试", nodewire.Place(nodeKey))
		}
	}
	if !local {
		return "", fmt.Errorf("还不知道 %s 的工作区目录", nodewire.Place(nodeKey))
	}
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	return a.Cfg.LocalWorkspaceRoot(), nil
}

// projectPath resolves a project's directory on one machine.
func (a *Service) projectPath(ctx context.Context, nodeKey, dir string) (string, error) {
	clean, err := CheckProjectDir(dir)
	if err != nil {
		return "", err
	}
	root, err := a.workspaceRootOf(ctx, nodeKey)
	if err != nil {
		return "", err
	}
	return path.Join(nodewire.ProjectsDir(root), clean), nil
}

// makeProjectDir creates the directory a project was given, on whichever
// machine holds it. A directory that is not there yet is the normal case:
// the owner names a project, the machine makes room for it.
func (a *Service) makeProjectDir(ctx context.Context, nodeKey, dir string) error {
	if a.localMachine(nodeKey) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("建立目录 %s 失败：%w", dir, err)
		}
		return nil
	}
	if a.Nodes == nil {
		return fmt.Errorf("还不能在 %s 上建立目录", nodewire.Place(nodeKey))
	}
	if _, err := a.Nodes.Files(ctx, nodeKey, nodewire.FileRequest{Op: nodewire.FileMkdir, Path: dir}); err != nil {
		return fmt.Errorf("在 %s 上建立目录 %s 失败：%w", nodewire.Place(nodeKey), dir, err)
	}
	return nil
}
