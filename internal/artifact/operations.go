package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/ops"
)

// MergeConflict is the node operation's conflict result. Store translates it
// into its existing landing state; Repo.Merge retains its public path result.
type MergeConflict struct{ Paths []string }

func (e MergeConflict) Error() string { return "merge conflicts: " + strings.Join(e.Paths, ", ") }

// GitError preserves the git status locally and across the node protocol.
// Code is -1 if git could not be started. Stderr is diagnostic only.
type GitError struct {
	Command string
	Code    int
	Stderr  string
	cause   error
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", e.Command, e.Code, strings.TrimSpace(e.Stderr))
}
func (e *GitError) ExitCode() int { return e.Code }
func (e *GitError) Unwrap() error { return e.cause }

// EncodeFailure and DecodeFailure preserve domain errors over JSON. Unknown
// failures remain typed protocol failures with their original diagnostic.
func EncodeFailure(err error) *ops.Failure {
	if err == nil {
		return nil
	}
	f := &ops.Failure{Code: "operation", Message: err.Error()}
	var limit TooLarge
	var conflict MergeConflict
	var gitErr *GitError
	var wire *ops.Failure
	switch {
	case errors.As(err, &limit):
		f.Code, f.Which, f.Have, f.Limit = "too_large", limit.Which, limit.Have, limit.Limit
	case errors.As(err, &conflict):
		f.Code, f.Paths = "merge_conflict", conflict.Paths
	case errors.Is(err, context.Canceled):
		f.Code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		f.Code = "deadline"
	case errors.As(err, &gitErr):
		f.Code, f.Command, f.ExitCode, f.Stderr = "git", gitErr.Command, gitErr.Code, gitErr.Stderr
	case errors.As(err, &wire):
		return wire
	case errors.Is(err, os.ErrNotExist):
		f.Code = "not_exist"
	case errors.Is(err, os.ErrExist):
		f.Code = "exists"
	case errors.Is(err, os.ErrPermission):
		f.Code = "permission"
	}
	return f
}

func DecodeFailure(f *ops.Failure) error {
	if f == nil {
		return nil
	}
	switch f.Code {
	case "too_large":
		return TooLarge{Which: f.Which, Have: f.Have, Limit: f.Limit}
	case "merge_conflict":
		return MergeConflict{Paths: f.Paths}
	case "git":
		return &GitError{Command: f.Command, Code: f.ExitCode, Stderr: f.Stderr}
	case "canceled":
		return fmt.Errorf("%s: %w", f.Message, context.Canceled)
	case "deadline":
		return fmt.Errorf("%s: %w", f.Message, context.DeadlineExceeded)
	case "not_exist":
		return fmt.Errorf("%s: %w", f.Message, os.ErrNotExist)
	case "exists":
		return fmt.Errorf("%s: %w", f.Message, os.ErrExist)
	case "permission":
		return fmt.Errorf("%s: %w", f.Message, os.ErrPermission)
	default:
		return f
	}
}

// RunOperation is shared by the node server, LocalNodes and hub recovery.
// Repo owns the git algorithms; this dispatch adds no shell or second copy.
func RunOperation(ctx context.Context, req ops.Request) (ops.Result, error) {
	var result ops.Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateOperation(req); err != nil {
		return result, &ops.Failure{Code: "invalid_request", Message: err.Error()}
	}
	r := &Repo{Dir: req.Repo, Limits: Limits(req.Limits)}
	var err error
	switch req.Op {
	case ops.Init:
		_, err = Open(ctx, req.Repo)
	case ops.Snapshot:
		result.Commit, result.Changed, err = r.Snapshot(ctx, req.WorkTree, req.Parent, req.Message, req.Flatten)
	case ops.Checkout:
		err = r.Checkout(ctx, req.Commit, req.WorkTree)
	case ops.Has:
		// Missing commits are a negative answer; process/start failures are errors.
		_, err = r.git(ctx, nil, "cat-file", "-e", req.Commit+"^{commit}")
		result.Has = err == nil
		var exit *GitError
		if errors.As(err, &exit) && exit.Code == 128 {
			// Distinguish an absent object from an absent/unusable repository.
			_, err = r.git(ctx, nil, "rev-parse", "--git-dir")
		}
	case ops.Bundle:
		if err = os.MkdirAll(filepath.Dir(req.Path), 0o700); err == nil {
			err = r.Bundle(ctx, req.Path, req.Commit, req.Have)
		}
	case ops.Unbundle:
		err = r.Unbundle(ctx, req.Path)
	case ops.Merge:
		var conflicts []string
		if req.LegacyMerge {
			result.Commit, conflicts, err = r.mergeLegacy(ctx, req.Base, req.Ours, req.Theirs, req.Message)
		} else {
			result.Commit, conflicts, err = r.Merge(ctx, req.Base, req.Ours, req.Theirs, req.Message)
		}
		if err == nil && len(conflicts) > 0 {
			err = MergeConflict{Paths: conflicts}
		}
	case ops.Apply:
		result.Paths, err = r.Apply(ctx, req.From, req.Commit, req.WorkTree)
	case ops.Changed:
		result.Paths, err = r.Changed(ctx, req.From, req.Commit)
	case ops.Remove:
		err = os.RemoveAll(req.Path)
	case ops.ListWorktrees:
		var entries []os.DirEntry
		entries, err = os.ReadDir(req.WorkTree)
		if os.IsNotExist(err) {
			err = nil
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				break
			}
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "wt-") {
				continue
			}
			info, statErr := entry.Info()
			if statErr == nil && !info.ModTime().After(req.Before) {
				result.Paths = append(result.Paths, filepath.Join(req.WorkTree, entry.Name()))
			}
		}
	case ops.PathState:
		result.State, err = r.pathState(ctx, req.WorkTree, req.From, req.Commit, req.Path)
	case ops.WritePath:
		err = r.writePath(ctx, req.WorkTree, req.Commit, req.Path)
	}
	return result, err
}

func validateOperation(req ops.Request) error {
	var paths []string
	var commits []string
	switch req.Op {
	case ops.Init:
		paths = []string{req.Repo}
	case ops.Snapshot:
		paths = []string{req.Repo, req.WorkTree}
		if req.Parent != "" {
			commits = []string{req.Parent}
		}
	case ops.Checkout:
		paths, commits = []string{req.Repo, req.WorkTree}, []string{req.Commit}
	case ops.Has:
		paths, commits = []string{req.Repo}, []string{req.Commit}
	case ops.Bundle:
		paths, commits = []string{req.Repo, req.Path}, []string{req.Commit}
		for _, have := range req.Have {
			if have != "" {
				commits = append(commits, have)
			}
		}
	case ops.Unbundle:
		paths = []string{req.Repo, req.Path}
	case ops.Merge:
		paths, commits = []string{req.Repo}, []string{req.Base, req.Ours, req.Theirs}
	case ops.Apply, ops.PathState:
		paths, commits = []string{req.Repo, req.WorkTree}, []string{req.From, req.Commit}
	case ops.Changed:
		paths, commits = []string{req.Repo}, []string{req.Commit}
		if req.From != "" {
			commits = append(commits, req.From)
		}
	case ops.WritePath:
		paths, commits = []string{req.Repo, req.WorkTree}, []string{req.Commit}
	case ops.Remove:
		paths = []string{req.Path}
		if filepath.Clean(req.Path) == string(filepath.Separator) {
			return fmt.Errorf("artifact: cannot remove a filesystem root")
		}
	case ops.ListWorktrees:
		paths = []string{req.WorkTree}
		if req.Before.IsZero() {
			return fmt.Errorf("artifact: missing sweep cutoff")
		}
	default:
		return fmt.Errorf("artifact: unknown operation %q", req.Op)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return fmt.Errorf("artifact: path %q must be absolute", path)
		}
	}
	for _, sha := range commits {
		if !shaPattern.MatchString(sha) {
			return fmt.Errorf("artifact: %q is not a commit id", sha)
		}
	}
	if req.Op == ops.PathState || req.Op == ops.WritePath {
		if !filepath.IsLocal(req.Path) || filepath.Clean(req.Path) == "." || strings.ContainsRune(req.Path, 0) {
			return fmt.Errorf("artifact: %q is not a tree path", req.Path)
		}
	}
	return nil
}

func (s *Store) operation(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if node == "" {
		return RunOperation(ctx, req)
	}
	return s.nodes.Artifact(ctx, node, req)
}
