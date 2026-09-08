package artifact

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// EmptyTree is git's well-known empty tree: the "before" of a first
// snapshot.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Change is one path that differs between two snapshots.
type Change struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A | M | D
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// Limits on what a review may cost: an index is at most this many
// entries, a file's diff at most this many bytes, and git gets this long.
const (
	MaxChanges           = 500
	MaxDiffBytes         = 200 * 1024
	DefaultReviewTimeout = 30 * time.Second
)

// Changes indexes what differs from one snapshot to the next: status
// and line counts per path, renames not detected (a rename is a D and
// an A). Truncated says the index stopped at MaxChanges.
func (r *Repo) Changes(ctx context.Context, from, to string) ([]Change, bool, error) {
	if from == "" {
		from = EmptyTree
	}
	ctx, cancel := context.WithTimeout(ctx, r.Review.defaults().Timeout)
	defer cancel()
	status, err := r.git(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-status", from, to)
	if err != nil {
		return nil, false, err
	}
	numstat, err := r.git(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--numstat", from, to)
	if err != nil {
		return nil, false, err
	}
	counts := map[string]Change{}
	for _, rec := range strings.Split(numstat, "\x00") {
		// "<added>\t<deleted>\t<path>", "-" for a binary file
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		c := Change{Path: parts[2]}
		if parts[0] == "-" || parts[1] == "-" {
			c.Binary = true
		} else {
			added, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, false, fmt.Errorf("numstat of %s: %w", c.Path, err)
			}
			deleted, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, false, fmt.Errorf("numstat of %s: %w", c.Path, err)
			}
			c.Added, c.Deleted = added, deleted
		}
		counts[c.Path] = c
	}
	var out []Change
	fields := strings.Split(status, "\x00")
	truncated := false
	for i := 0; i+1 < len(fields); i += 2 {
		st, path := fields[i], fields[i+1]
		if st == "" || path == "" {
			continue
		}
		if len(out) >= r.Review.defaults().MaxChanges {
			truncated = true
			break
		}
		c := counts[path]
		c.Path, c.Status = path, st[:1]
		out = append(out, c)
	}
	return out, truncated, nil
}

// FileDiff is one path's unified diff between two snapshots, cut at
// MaxDiffBytes: the process is stopped once the limit is read, so a
// huge file costs the limit, not the file.
func (r *Repo) FileDiff(ctx context.Context, from, to, path string) (string, bool, error) {
	if from == "" {
		from = EmptyTree
	}
	if path == "" || strings.HasPrefix(path, "-") {
		return "", false, errors.New("a path is required")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Review.defaults().Timeout)
	defer cancel()
	raw, truncated, err := runBounded(ctx, r.Dir, r.Review.defaults().MaxDiffBytes, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", from, to, "--", path)
	if err != nil {
		return "", false, err
	}
	return string(raw), truncated, nil
}

// runBounded runs git with the repository as GIT_DIR and returns at most
// max bytes of stdout; past the limit the process is killed, so a huge
// output costs the limit, not the output.
func runBounded(ctx context.Context, gitDir string, max int, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_DIR="+gitDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(bufio.NewReader(stdout), int64(max)+1))
	truncated := len(raw) > max
	if truncated {
		raw = raw[:max]
		// Past the limit the rest of the output is unwanted; Wait below
		// collects the process whether or not the kill was delivered.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, false, readErr
	}
	if waitErr != nil && !truncated {
		return nil, false, fmt.Errorf("git %s: %w: %s", args[0], waitErr, strings.TrimSpace(stderr.String()))
	}
	return raw, truncated, nil
}

// ---------------------------------------------------------------- store

// Changes is the index of an attempt's change, by project: what the
// snapshot pair differs in. A sealed project whose objects stay on its
// own machine has nothing the hub may show.
func (s *Store) Changes(ctx context.Context, projectID, from, to string) ([]Change, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, from, to)
	if err != nil {
		return nil, false, err
	}
	return repo.Changes(ctx, from, to)
}

// FileDiff is one file of that change. The path must be in the index:
// nothing is diffed that the index did not name.
func (s *Store) FileDiff(ctx context.Context, projectID, from, to, path string) (string, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, from, to)
	if err != nil {
		return "", false, err
	}
	changes, _, err := repo.Changes(ctx, from, to)
	if err != nil {
		return "", false, err
	}
	known := false
	for _, c := range changes {
		if c.Path == path {
			known = true
			if c.Binary {
				return "", false, fmt.Errorf("%s is binary; no text diff", path)
			}
			break
		}
	}
	if !known {
		return "", false, fmt.Errorf("%s is not among the changed files", path)
	}
	return repo.FileDiff(ctx, from, to, path)
}

func (s *Store) reviewRepo(ctx context.Context, projectID, from, to string) (*Repo, error) {
	p, ok, err := s.projects.GetHistorical(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no project %q", projectID)
	}
	if metadataOnly(p) {
		return nil, fmt.Errorf("project %s is sealed and its data stays on %s; no diff here", p.ID, p.Home.Node)
	}
	for _, sha := range []string{from, to} {
		if sha != "" && !shaPattern.MatchString(sha) {
			return nil, fmt.Errorf("bad snapshot id %q", sha)
		}
	}
	if to == "" {
		return nil, errors.New("no after-snapshot: nothing changed, or the change was not captured")
	}
	r, err := s.Repo(ctx, projectID)
	if err == nil {
		r.Review = s.Review.defaults()
	}
	return r, err
}
