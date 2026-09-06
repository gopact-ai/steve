package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// mergeLegacy uses an isolated repository for git merge on older git. Its
// object directory is the shadow's, but HEAD, index and merge state are private.
// In particular this never invokes the git-merge-one-file shell helper.
func (r *Repo) mergeLegacy(ctx context.Context, base, ours, theirs, message string) (string, []string, error) {
	if ours == base {
		return theirs, nil, nil
	}
	if theirs == base {
		return ours, nil, nil
	}
	dir, err := os.MkdirTemp("", "steve-merge-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)
	if _, err := git(ctx, "", nil, "init", "--quiet", dir); err != nil {
		return "", nil, err
	}
	objects, err := filepath.Abs(filepath.Join(r.Dir, "objects"))
	if err != nil {
		return "", nil, err
	}
	env := []string{"GIT_WORK_TREE=" + dir, "GIT_INDEX_FILE=" + filepath.Join(dir, ".git", "index"), "GIT_OBJECT_DIRECTORY=" + objects}
	run := func(args ...string) (string, error) { return git(ctx, filepath.Join(dir, ".git"), env, args...) }
	if _, err := run("update-ref", "HEAD", ours); err != nil {
		return "", nil, err
	}
	if _, err := run("read-tree", "--reset", "-u", ours); err != nil {
		return "", nil, err
	}
	// Snapshots descend from base, as in the merge-tree path. A private,
	// empty hooks directory also avoids inheriting the operator's hooks.
	hooks := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return "", nil, err
	}
	_, mergeErr := run("-c", "user.name=steve", "-c", "user.email=steve@localhost", "-c", "core.hooksPath="+hooks,
		"merge", "--no-commit", "--no-ff", "--no-edit", theirs)
	if mergeErr != nil {
		var exit *GitError
		if !errors.As(mergeErr, &exit) || exit.Code != 1 {
			return "", nil, mergeErr
		}
		out, err := run("ls-files", "--unmerged", "-z")
		if err != nil {
			return "", nil, err
		}
		var conflicts []string
		seen := map[string]bool{}
		for _, entry := range strings.Split(out, "\x00") {
			if _, path, ok := strings.Cut(entry, "\t"); ok && !seen[path] {
				conflicts = append(conflicts, path)
				seen[path] = true
			}
		}
		if len(conflicts) == 0 {
			return "", nil, mergeErr
		}
		return "", conflicts, nil
	}
	tree, err := run("write-tree")
	if err != nil {
		return "", nil, err
	}
	out, err := r.git(ctx, nil, "commit-tree", strings.TrimSpace(tree), "-m", message, "-p", ours, "-p", theirs)
	if err != nil {
		return "", nil, err
	}
	sha := strings.TrimSpace(out)
	return sha, nil, r.pin(ctx, sha)
}
