package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
)

// ErrWorkspacePreparing says a project's directory on a machine is being
// made right now. The work that asked for it can be tried again once the
// copy is there; nothing is lost in the meantime.
var ErrWorkspacePreparing = errors.New("项目的工作区还在准备中")

// attachWait is how long a caller waits for a copy that has to be cloned
// before it is told to come back. A directory that only has to be made
// is ready inside the declaration itself, so the wait is for clones.
const attachWait = 90 * time.Second

// EnsureProjectWorkspace gives a project a directory on the machine that
// is about to work in it. A project the machine already holds — its home
// or a ready copy — is left alone. Anything else is attached the way the
// page would attach it: the project's own directory name under that
// machine's workspace, cloned from the project's remote when there is one
// and the directory is not there yet, adopted otherwise.
//
// This is what keeps "the project is over there" from being the owner's
// problem: an agent is picked for what it can do, and the project follows
// it to the machine it runs on.
func (a *Service) EnsureProjectWorkspace(ctx context.Context, projectID, nodeKey string) error {
	if a.Projects == nil {
		return nil
	}
	p, ok, err := a.Projects.Get(ctx, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("没有叫 %q 的项目", projectID)
	}
	if _, err := p.Place(nodeKey); err == nil {
		return nil
	}
	if existing, found := p.CopyOn(nodeKey); found {
		switch existing.State {
		case project.CopyProvisioning:
			return a.awaitCopy(ctx, projectID, nodeKey)
		case project.CopyFailed:
			return fmt.Errorf("%s 上的项目副本没有建成：%s", nodewire.Name(nodeKey), existing.Error)
		}
	}
	origin := "adopt"
	if source := a.cloneSource(p); source != "" {
		dir, err := a.projectDirName(ctx, projectID)
		if err != nil {
			return err
		}
		path, err := a.projectPath(ctx, nodeKey, dir)
		if err != nil {
			return err
		}
		empty, err := a.missingDir(ctx, nodeKey, path)
		if err != nil {
			return err
		}
		if empty {
			origin = "clone"
		}
	}
	if err := a.AddWorkspace(ctx, projectID, consoleapi.AddWorkspaceRequest{Node: nodeKey, Origin: origin}); err != nil {
		return err
	}
	if origin == "adopt" {
		return nil
	}
	return a.awaitCopy(ctx, projectID, nodeKey)
}

// missingDir reports whether a directory is not there yet, which is what
// a clone needs. A machine that cannot answer is not guessed about.
func (a *Service) missingDir(ctx context.Context, nodeKey, path string) (bool, error) {
	if nodeKey != "" && a.Nodes == nil {
		return false, fmt.Errorf("还不能查看 %s 上的目录", nodewire.Name(nodeKey))
	}
	found, err := a.inspect(ctx, nodeKey, path)
	if err != nil {
		return false, fmt.Errorf("检查 %s 上的 %s 失败：%w", nodewire.Name(nodeKey), path, err)
	}
	return len(found) == 1 && found[0].Missing, nil
}

// awaitCopy waits for a copy being cloned to settle. It returns nil once
// the copy is usable, the clone's own failure when it did not come up,
// and ErrWorkspacePreparing while it is still running.
func (a *Service) awaitCopy(ctx context.Context, projectID, nodeKey string) error {
	deadline := time.Now().Add(attachWait)
	for {
		p, ok, err := a.Projects.Get(ctx, projectID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("没有叫 %q 的项目", projectID)
		}
		made, found := p.CopyOn(nodeKey)
		switch {
		case !found:
			return fmt.Errorf("%s 上的项目副本不见了", nodewire.Name(nodeKey))
		case made.State == project.CopyReady:
			return nil
		case made.State == project.CopyFailed:
			return fmt.Errorf("%s 上的项目副本没有建成：%s", nodewire.Name(nodeKey), made.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w：正在把 %s 复制到 %s，完成后再说一次", ErrWorkspacePreparing, projectID, nodewire.Name(nodeKey))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
