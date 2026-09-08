// Package artifact is the content-addressed layer: every result Steve
// binds a name to is a git commit in a bare repository per project, and
// every directory Steve runs an attempt in is materialised from one.
//
// The repository is a shadow of the user's directory, never the user's own
// .git: a snapshot is taken with a throwaway index against a work tree, so
// a directory that is not a git repository — or is one with its own
// history — is versioned without being touched. Git is the transport too:
// a bundle carries a commit's closure to a node and a result back.
package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Repo is one project's bare shadow repository.
type Repo struct {
	Review ReviewLimits
	Dir    string
	Limits Limits
}

// openMu serialises repository creation: two attempts materialising the
// same project at once must not both run git init on the same directory.
var openMu sync.Mutex

// Open opens or creates the bare repository at dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	openMu.Lock()
	defer openMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return &Repo{Dir: dir}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if _, err := git(ctx, "", nil, "init", "--bare", "--quiet", dir); err != nil {
		return nil, err
	}
	r := &Repo{Dir: dir}
	// Snapshots must not depend on who runs the hub, and nothing may keep
	// working in a shadow repository after the operation that used it
	// returned: git's automatic maintenance detaches into the background,
	// which is a process nobody waits for and a directory that is not
	// free when its owner removes it.
	for _, setting := range [][2]string{{"user.name", "steve"}, {"user.email", "steve@localhost"}, {"gc.auto", "0"}, {"gc.autoDetach", "false"}, {"maintenance.auto", "false"}} {
		if _, err := r.git(ctx, nil, "config", setting[0], setting[1]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Snapshot commits the current contents of workTree, with parent as the
// previous snapshot (empty for the first). It returns the commit and
// whether anything changed against the parent's tree; an unchanged tree
// returns the parent itself rather than an empty commit.
//
// flatten says the directory is the platform's own — a worktree made
// for one attempt — so a repository an agent started inside it is
// noise to remove. A user's directory is never modified: a nested
// repository there is left alone and simply not part of the snapshot.
func (r *Repo) Snapshot(ctx context.Context, workTree, parent, message string, flatten bool) (sha string, changed bool, err error) {
	// A shell's cd followed a symlink at the workspace root. Resolve that
	// root for Go's walk too, while leaving symlinks inside it untouched.
	workTree, err = filepath.EvalSymlinks(workTree)
	if err != nil {
		return "", false, err
	}
	index, cleanup, err := r.tempIndex()
	if err != nil {
		return "", false, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + workTree}
	if parent != "" {
		if _, err := r.git(ctx, env, "read-tree", parent); err != nil {
			return "", false, fmt.Errorf("read parent tree: %w", err)
		}
	}
	add := []string{"add", "-A", "--", "."}
	if flatten {
		flattened, err := flattenNestedRepos(ctx, workTree)
		if err != nil {
			return "", false, err
		}
		if len(flattened) > 0 {
			log.Printf("artifact: %s: flattened nested git repositories at %s", workTree, strings.Join(flattened, ", "))
		}
	} else {
		nested, err := nestedRepos(workTree)
		if err != nil {
			return "", false, err
		}
		// Left alone and left out: git would otherwise record them as
		// links, or refuse one that has no commit yet.
		if len(nested) > 0 {
			log.Printf("artifact: %s: nested git repositories left out of the snapshot: %s", workTree, strings.Join(nested, ", "))
		}
		for _, dir := range nested {
			add = append(add, ":(exclude,literal)"+filepath.ToSlash(dir))
		}
	}
	if err := r.prepareSnapshotIndex(ctx, workTree, env, add[3:]); err != nil {
		return "", false, err
	}
	if _, err := r.git(ctx, env, add...); err != nil {
		return "", false, fmt.Errorf("stage %s: %w", workTree, err)
	}
	// A nested repository staged as a gitlink would land as an empty
	// directory; it is not this snapshot's to carry.
	if links, err := r.gitlinks(ctx, env); err != nil {
		return "", false, err
	} else if len(links) > 0 {
		log.Printf("artifact: %s: nested git repositories left out of the snapshot: %s", workTree, strings.Join(links, ", "))
		if _, err := r.git(ctx, env, append([]string{"update-index", "--force-remove", "--"}, links...)...); err != nil {
			return "", false, fmt.Errorf("drop gitlinks: %w", err)
		}
	}
	tree, err := r.git(ctx, env, "write-tree")
	if err != nil {
		return "", false, err
	}
	tree = strings.TrimSpace(tree)
	if parent != "" {
		parentTree, err := r.git(ctx, nil, "rev-parse", parent+"^{tree}")
		if err != nil {
			return "", false, err
		}
		if strings.TrimSpace(parentTree) == tree {
			return parent, false, nil
		}
	}
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	out, err := r.git(ctx, nil, args...)
	if err != nil {
		return "", false, err
	}
	sha = strings.TrimSpace(out)
	return sha, true, r.pin(ctx, sha)
}

// pin gives a commit a ref so it is an artifact git will keep, and a name
// a bundle can carry.
// gitlinks lists the submodule entries in the index.
func (r *Repo) gitlinks(ctx context.Context, env []string) ([]string, error) {
	out, err := r.git(ctx, env, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, fmt.Errorf("list index: %w", err)
	}
	var links []string
	for _, line := range strings.Split(out, "\x00") {
		if !strings.HasPrefix(line, "160000 ") {
			continue
		}
		if _, path, ok := strings.Cut(line, "\t"); ok {
			links = append(links, path)
		}
	}
	return links, nil
}

// nestedRepos lists the directories below the top level that hold a
// .git, relative to workTree, without touching them.
func nestedRepos(workTree string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(workTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == workTree || !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		rel, _ := filepath.Rel(workTree, path)
		found = append(found, rel)
		return filepath.SkipDir
	})
	return found, err
}

// flattenNestedRepos removes any .git below the top level of workTree
// and returns where they were. An agent that ran git init inside its
// directory did not make a submodule: git would record the directory as
// a bare link and the files it wrote would never reach the project. The
// files are what was asked for; the repository around them is not.
func flattenNestedRepos(ctx context.Context, workTree string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(workTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == workTree {
			return nil
		}
		if d.Name() != ".git" {
			return nil
		}
		if filepath.Dir(path) == workTree {
			if d.IsDir() {
				return filepath.SkipDir // the worktree's own, if any: git ignores it
			}
			return nil
		}
		rel, _ := filepath.Rel(workTree, filepath.Dir(path))
		found = append(found, rel)
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return found, err
}

func (r *Repo) pin(ctx context.Context, sha string) error {
	_, err := r.git(ctx, nil, "update-ref", RefFor(sha), sha)
	return err
}

// RefFor is the ref under which an artifact is kept.
func RefFor(sha string) string { return "refs/steve/artifacts/" + sha }

// Checkout materialises a commit's tree into dir, which must be empty or
// absent: an attempt's directory starts from exactly the base and nothing
// else.
func (r *Repo) Checkout(ctx context.Context, sha, dir string) error {
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("artifact: %q is not a commit id", sha)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("artifact: %s is not empty", dir)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	index, cleanup, err := r.tempIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + dir}
	if _, err := r.git(ctx, env, "read-tree", "--reset", "-u", sha); err != nil {
		return fmt.Errorf("checkout %s into %s: %w", short(sha), dir, err)
	}
	return nil
}

// Apply brings dir to exactly the tree of sha, adding, rewriting and
// deleting as needed, and returns the paths it touched. It is the write
// half of a landing: dir is the canonical workspace, and the caller holds
// its lock.
func (r *Repo) Apply(ctx context.Context, from, sha, dir string) ([]string, error) {
	paths, err := r.Changed(ctx, from, sha)
	if err != nil {
		return nil, err
	}
	index, cleanup, err := r.tempIndex()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + dir}
	if _, err := r.git(ctx, env, "read-tree", from); err != nil {
		return nil, err
	}
	// The fresh index has no stat data; refreshing it against the work
	// tree is what lets the two-tree merge tell "unchanged" from "edited".
	if _, err := r.git(ctx, env, "update-index", "--refresh", "-q", "--ignore-missing"); err != nil {
		return nil, fmt.Errorf("refresh %s: %w", dir, err)
	}
	if _, err := r.git(ctx, env, "read-tree", "-m", "-u", from, sha); err != nil {
		return nil, fmt.Errorf("apply %s..%s to %s: %w", short(from), short(sha), dir, err)
	}
	return paths, nil
}

// Changed lists the paths that differ between two commits.
func (r *Repo) Changed(ctx context.Context, from, to string) ([]string, error) {
	var out string
	var err error
	if from == "" {
		out, err = r.git(ctx, nil, "ls-tree", "-r", "--name-only", "-z", to)
	} else {
		out, err = r.git(ctx, nil, "diff-tree", "-r", "--name-only", "--no-commit-id", "-z", from, to)
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\x00") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// Merge three-way merges ours and theirs against base without a work
// tree and returns the merged commit, or the conflicting paths.
func (r *Repo) Merge(ctx context.Context, base, ours, theirs, message string) (sha string, conflicts []string, err error) {
	if ours == base {
		return theirs, nil, nil
	}
	if theirs == base {
		return ours, nil, nil
	}
	// Both sides descend from base — snapshots always name their parent —
	// so git finds that base itself; naming it needs git 2.40, and the
	// nodes are not all there yet.
	out, err := r.git(ctx, nil, "merge-tree", "--write-tree", "--name-only", "-z", ours, theirs)
	lines := strings.Split(out, "\x00")
	if err != nil {
		var exit *GitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(lines) > 1 {
			// Git's -z format is tree, paths, empty field, then messages.
			var paths []string
			for _, line := range lines[1:] {
				if line == "" {
					break
				}
				paths = append(paths, line)
			}
			if len(paths) > 0 {
				return "", paths, nil
			}
		}
		return "", nil, err
	}
	tree := strings.TrimSpace(lines[0])
	commit, err := r.git(ctx, nil, "commit-tree", tree, "-m", message, "-p", ours, "-p", theirs)
	if err != nil {
		return "", nil, err
	}
	sha = strings.TrimSpace(commit)
	return sha, nil, r.pin(ctx, sha)
}

// Bundle writes a bundle carrying sha and its closure, minus what the
// receiver already has, to path.
func (r *Repo) Bundle(ctx context.Context, path, sha string, have []string) error {
	if err := r.pin(ctx, sha); err != nil {
		return err
	}
	args := []string{"bundle", "create", path, RefFor(sha)}
	for _, h := range have {
		if h != "" {
			args = append(args, "^"+h)
		}
	}
	_, err := r.git(ctx, nil, args...)
	return err
}

// Unbundle fetches everything in a bundle into the repository.
func (r *Repo) Unbundle(ctx context.Context, path string) error {
	out, err := r.git(ctx, nil, "bundle", "list-heads", path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, err := r.git(ctx, nil, "fetch", "--quiet", path, fields[1]+":"+fields[1]); err != nil {
			return err
		}
	}
	return nil
}

// Has reports whether the repository holds the commit.
func (r *Repo) Has(ctx context.Context, sha string) bool {
	_, err := r.git(ctx, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// Parents returns a commit's parents.
func (r *Repo) Parents(ctx context.Context, sha string) ([]string, error) {
	out, err := r.git(ctx, nil, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) < 1 {
		return nil, nil
	}
	return fields[1:], nil
}

func (r *Repo) tempIndex() (string, func(), error) {
	f, err := os.CreateTemp("", "steve-index-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return name, func() { os.Remove(name) }, nil
}

func (r *Repo) git(ctx context.Context, env []string, args ...string) (string, error) {
	return git(ctx, r.Dir, env, args...)
}

func git(ctx context.Context, gitDir string, env []string, args ...string) (string, error) {
	return gitInput(ctx, gitDir, env, nil, args...)
}

func gitInput(ctx context.Context, gitDir string, env []string, input io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = input
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if gitDir != "" {
		cmd.Env = append(cmd.Env, "GIT_DIR="+gitDir)
	}
	cmd.Env = append(cmd.Env, env...)
	// Pathspecs are relative to the current directory, so a command that
	// works against a work tree runs inside it.
	for _, kv := range env {
		if dir, ok := strings.CutPrefix(kv, "GIT_WORK_TREE="); ok {
			cmd.Dir = dir
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.String(), ctx.Err()
		}
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.String(), &GitError{Command: args[0], Code: code, Stderr: detail, cause: err}
	}
	return stdout.String(), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
