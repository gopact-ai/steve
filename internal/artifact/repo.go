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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Repo is one project's bare shadow repository.
type Repo struct {
	Dir string
}

// Open opens or creates the bare repository at dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
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
	// Snapshots must not depend on who runs the hub.
	_, _ = r.git(ctx, nil, "config", "user.name", "steve")
	_, _ = r.git(ctx, nil, "config", "user.email", "steve@localhost")
	return r, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Snapshot commits the current contents of workTree, with parent as the
// previous snapshot (empty for the first). It returns the commit and
// whether anything changed against the parent's tree; an unchanged tree
// returns the parent itself rather than an empty commit.
func (r *Repo) Snapshot(ctx context.Context, workTree, parent, message string) (sha string, changed bool, err error) {
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
	if _, err := r.git(ctx, env, "add", "-A", "--", "."); err != nil {
		return "", false, fmt.Errorf("stage %s: %w", workTree, err)
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
		out, err = r.git(ctx, nil, "ls-tree", "-r", "--name-only", to)
	} else {
		out, err = r.git(ctx, nil, "diff-tree", "-r", "--name-only", "--no-commit-id", from, to)
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
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
	out, err := r.git(ctx, nil, "merge-tree", "--write-tree", "--name-only", ours, theirs)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(lines) > 1 {
			// Output is the tree, the conflicted paths, a blank line, then
			// messages; only the paths are the answer.
			var paths []string
			for _, line := range lines[1:] {
				if line == "" {
					break
				}
				paths = append(paths, line)
			}
			return "", paths, nil
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
	cmd := exec.CommandContext(ctx, "git", args...)
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
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Script renders the same operations as shell for a node that has git but
// no Steve code for it: the hub is the authority and the node executes.
// Every argument is quoted; nothing from a user reaches this unquoted.
type Script struct{}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// InitScript creates the bare repository if absent.
func (Script) Init(dir string) string {
	return fmt.Sprintf("test -f %s/HEAD || (mkdir -p %s && git init --bare --quiet %s && git --git-dir=%s config user.name steve && git --git-dir=%s config user.email steve@localhost)",
		quote(dir), quote(filepath.Dir(dir)), quote(dir), quote(dir), quote(dir))
}

// Unbundle fetches a bundle's tips into the bare repository.
func (Script) Unbundle(dir, bundle string) string {
	return fmt.Sprintf("cd %s && for ref in $(git bundle list-heads %s | cut -d' ' -f2); do git --git-dir=%s fetch --quiet %s \"$ref:$ref\" || exit 1; done",
		quote(filepath.Dir(dir)), quote(bundle), quote(dir), quote(bundle))
}

// Checkout materialises a commit into an empty directory.
func (Script) Checkout(dir, sha, target string) string {
	return fmt.Sprintf("mkdir -p %s && [ -z \"$(ls -A %s)\" ] && GIT_DIR=%s GIT_WORK_TREE=%s GIT_INDEX_FILE=%s.index git read-tree --reset -u %s && rm -f %s.index",
		quote(target), quote(target), quote(dir), quote(target), quote(target), quote(sha), quote(target))
}

// Snapshot commits a directory's contents on top of parent and prints the
// commit id, or the parent's id when nothing changed.
func (Script) Snapshot(dir, workTree, parent, message string) string {
	parentArg := ""
	readParent := "true"
	if parent != "" {
		parentArg = " -p " + quote(parent)
		readParent = "git read-tree " + quote(parent)
	}
	compare := "false"
	if parent != "" {
		compare = fmt.Sprintf("[ \"$tree\" = \"$(git rev-parse %s^{tree})\" ]", quote(parent))
	}
	return fmt.Sprintf("export GIT_DIR=%s GIT_WORK_TREE=%s GIT_INDEX_FILE=%s.index; cd \"$GIT_WORK_TREE\" && rm -f \"$GIT_INDEX_FILE\"; %s && git add -A -- . && tree=$(git write-tree) && rm -f \"$GIT_INDEX_FILE\" && if %s; then echo %s; else sha=$(git commit-tree \"$tree\" -m %s%s) && git update-ref \"refs/steve/artifacts/$sha\" \"$sha\" && echo \"$sha\"; fi",
		quote(dir), quote(workTree), quote(workTree), readParent, compare, quote(parent), quote(message), parentArg)
}

// Merge three-way merges two commits at the node and prints the merged
// commit, or "CONFLICT" followed by the conflicting paths with exit 1.
func (Script) Merge(dir, ours, theirs, message string) string {
	return fmt.Sprintf("export GIT_DIR=%s; out=$(git merge-tree --write-tree --name-only %s %s); rc=$?; if [ $rc -eq 1 ]; then echo CONFLICT; echo \"$out\" | sed -n '2,/^$/p' | sed '/^$/d'; exit 1; fi; [ $rc -eq 0 ] || exit $rc; tree=$(echo \"$out\" | head -1); sha=$(git commit-tree \"$tree\" -m %s -p %s -p %s) && git update-ref \"refs/steve/artifacts/$sha\" \"$sha\" && echo \"$sha\"",
		quote(dir), quote(ours), quote(theirs), quote(message), quote(ours), quote(theirs))
}

// Apply brings a directory from one tree to another, as Repo.Apply does.
func (Script) Apply(dir, from, to, target string) string {
	return fmt.Sprintf("export GIT_DIR=%s GIT_WORK_TREE=%s GIT_INDEX_FILE=%s.land-index; cd \"$GIT_WORK_TREE\" && rm -f \"$GIT_INDEX_FILE\" && git read-tree %s && git update-index --refresh -q --ignore-missing; git read-tree -m -u %s %s; rc=$?; rm -f \"$GIT_INDEX_FILE\"; exit $rc",
		quote(dir), quote(target), quote(target), quote(from), quote(from), quote(to))
}

// Bundle writes a commit's closure, minus have, to path.
func (Script) Bundle(dir, path, sha string, have []string) string {
	var exclude strings.Builder
	for _, h := range have {
		if h != "" {
			exclude.WriteString(" ^" + quote(h))
		}
	}
	return fmt.Sprintf("git --git-dir=%s update-ref %s %s && git --git-dir=%s bundle create %s %s%s",
		quote(dir), quote(RefFor(sha)), quote(sha), quote(dir), quote(path), quote(RefFor(sha)), exclude.String())
}
