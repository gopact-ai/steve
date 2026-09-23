package artifact

import (
	"context"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

func (s *Store) VerifyPreparedWorkspace(ctx context.Context, ws project.Workspace, base string) error {
	if ws.Kind != project.KindWorktree || ws.Project == "" || ws.Path == "" || ws.Base != base {
		return gitrepo.ErrPreparedWorkspaceChanged
	}
	p, ok, err := s.projects.Get(ctx, ws.Project)
	if err != nil {
		return err
	}
	if !ok {
		return project.ErrUnknown
	}
	if err := s.admits(ctx, p, ws.Node); err != nil {
		return err
	}
	var bare string
	if ws.Node == "" {
		bare = filepath.Join(s.Dir, "objects", ws.Project+".git")
	} else {
		_, _, state, err := s.nodes.Git(ctx, ws.Node)
		if err != nil {
			return err
		}
		bare = nodeBare(state, ws.Project)
	}
	_, err = s.operation(ctx, ws.Node, ops.Request{Op: ops.VerifyCheckout, Repo: bare, WorkTree: ws.Path, Commit: base})
	return err
}
