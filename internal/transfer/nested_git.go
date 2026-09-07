package transfer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NestedGit retains repository history without copying repository-local
// configuration, hooks, credentials or pointers into a source worktree.
type NestedGit struct {
	Bundle []byte `json:"bundle,omitempty"`
	Head   string `json:"head,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func captureNestedGit(ctx context.Context, source string) (map[string]NestedGit, error) {
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	out := map[string]NestedGit{}
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Name() != ".git" {
			return nil
		}
		root := filepath.Dir(path)
		if root != source {
			top, err := git(ctx, root, "rev-parse", "--show-toplevel")
			if err != nil || strings.TrimSpace(string(top)) != root {
				return fmt.Errorf("cannot export nested Git repository %s: %v", root, err)
			}
			branch, _ := git(ctx, root, "symbolic-ref", "-q", "HEAD")
			repo := NestedGit{Branch: strings.TrimSpace(string(branch))}
			head, err := git(ctx, root, "rev-parse", "--verify", "HEAD")
			if err == nil {
				repo.Head = strings.TrimSpace(string(head))
				temp, err := os.MkdirTemp("", "steve-nested-history-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(temp)
				bundle := filepath.Join(temp, "history.bundle")
				if _, err := git(ctx, root, "bundle", "create", bundle, "--all", "HEAD"); err != nil {
					return err
				}
				repo.Bundle, err = os.ReadFile(bundle)
				if err != nil {
					return err
				}
			} else if repo.Branch == "" {
				return fmt.Errorf("nested Git repository has neither valid HEAD nor unborn branch: %s", root)
			}
			rel, err := filepath.Rel(source, root)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = repo
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}

func validateNestedGit(repos map[string]NestedGit) error {
	for path, repo := range repos {
		if path == "." || !filepath.IsLocal(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.Contains(path, "\\") {
			return fmt.Errorf("invalid nested repository path %q", path)
		}
		for _, part := range strings.Split(path, "/") {
			if part == ".git" {
				return fmt.Errorf("nested repository overlaps Git metadata")
			}
		}
		if repo.Branch != "" && !strings.HasPrefix(repo.Branch, "refs/heads/") {
			return fmt.Errorf("invalid nested repository branch")
		}
		if (len(repo.Bundle) == 0) != (repo.Head == "") || (repo.Head == "" && repo.Branch == "") {
			return fmt.Errorf("invalid nested repository history")
		}
	}
	return nil
}

func restoreNestedGit(ctx context.Context, repos map[string]NestedGit, home, stage string) error {
	paths := make([]string, 0, len(repos))
	for path := range repos {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		repo := repos[path]
		root := home
		for _, part := range strings.Split(path, "/") {
			root = filepath.Join(root, part)
			if err := transferDirectory(root); err != nil {
				return err
			}
		}
		if len(repo.Bundle) > 0 {
			if err := restoreGitHistory(ctx, repo.Bundle, repo.Head, repo.Branch, root, stage); err != nil {
				return fmt.Errorf("restore nested repository %s: %w", path, err)
			}
		} else {
			if _, err := git(ctx, root, "init", "--quiet"); err != nil {
				return err
			}
			if _, err := git(ctx, root, "symbolic-ref", "HEAD", repo.Branch); err != nil {
				return err
			}
		}
	}
	return nil
}
