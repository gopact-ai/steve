package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

var ErrPreparedWorkspaceChanged = errors.New("prepared recovery workspace is missing or changed")

// VerifyCheckout reads target files using a disposable index. It never resets
// the directory or discards user changes and includes ignored extra files.
func (r *Repo) VerifyCheckout(ctx context.Context, commit, dir string) error {
	if !shaPattern.MatchString(commit) {
		return ErrPreparedWorkspaceChanged
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return ErrPreparedWorkspaceChanged
	}
	index, cleanup, err := r.tempIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + dir}
	if _, err := r.git(ctx, env, "read-tree", commit); err != nil {
		return err
	}
	if _, err := r.git(ctx, env, "update-index", "--really-refresh"); err != nil {
		return fmt.Errorf("%w: %v", ErrPreparedWorkspaceChanged, err)
	}
	if _, err := r.git(ctx, env, "diff-files", "--quiet", "--"); err != nil {
		return fmt.Errorf("%w: %v", ErrPreparedWorkspaceChanged, err)
	}
	extra, err := r.git(ctx, env, "ls-files", "--others", "-z", "--")
	if err != nil {
		return err
	}
	if strings.Trim(extra, "\x00") != "" {
		return ErrPreparedWorkspaceChanged
	}
	tracked, err := r.git(ctx, env, "ls-files", "--cached", "-z", "--")
	if err != nil {
		return err
	}
	allowedDirs := map[string]bool{".": true}
	for _, name := range strings.Split(tracked, "\x00") {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			allowedDirs[parent] = true
		}
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPreparedWorkspaceChanged, err)
		}
		if entry.IsDir() {
			relative, err := filepath.Rel(dir, path)
			if err != nil || !allowedDirs[relative] {
				return ErrPreparedWorkspaceChanged
			}
		}
		return nil
	})
}

func (s *Store) VerifyPreparedWorkspace(ctx context.Context, ws project.Workspace, base string) error {
	if ws.Kind != project.KindWorktree || ws.Project == "" || ws.Path == "" || ws.Base != base {
		return ErrPreparedWorkspaceChanged
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
