package artifact

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type treeEntry struct{ mode, sha string }

// errNotAFile is a path a tree holds as a directory or a submodule.
var errNotAFile = errors.New("artifact: not a file")

func (r *Repo) entry(ctx context.Context, commit, path string) (treeEntry, error) {
	out, err := r.git(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, "ls-tree", "--full-tree", "-z", commit, "--", path)
	if err != nil || out == "" {
		return treeEntry{}, err
	}
	meta, _, ok := strings.Cut(out, "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 || fields[1] != "blob" {
		return treeEntry{}, fmt.Errorf("%w: %s in %s", errNotAFile, path, commit)
	}
	return treeEntry{fields[0], fields[2]}, nil
}

// dirEntry stands for a directory, on disk or in a tree, and unreadable
// for what is on disk but cannot be read as a file: neither is brought to
// a file's content one path at a time.
var (
	dirEntry   = treeEntry{mode: "040000"}
	unreadable = treeEntry{mode: "?"}
)

// pathState compares what dir holds at path with the path in the old and
// the merged tree: "merged", "old" when writing the merged content there
// finishes it, or "other". Only failing to reach dir or to run git is an
// error; whatever the workspace itself holds is an answer.
func (r *Repo) pathState(ctx context.Context, dir, from, to, path string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	current, writable, err := r.onDisk(ctx, root, path)
	if err != nil {
		return "", err
	}
	merged, err := r.entryOrDir(ctx, to, path)
	if err != nil {
		return "", err
	}
	old, err := r.entryOrDir(ctx, from, path)
	if err != nil {
		return "", err
	}
	switch {
	case current == merged:
		return "merged", nil
	case current == old && writable && merged != dirEntry:
		return "old", nil
	default:
		return "other", nil
	}
}

// onDisk reads path in root as a tree entry, and whether a file can be
// written there. Nothing there is the zero entry; a file standing where
// the path needs a directory makes it absent but not writable.
func (r *Repo) onDisk(ctx context.Context, root *os.Root, path string) (treeEntry, bool, error) {
	info, err := root.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return treeEntry{}, true, nil
	case errors.Is(err, syscall.ENOTDIR):
		return treeEntry{}, false, nil
	case err != nil:
		// Out of reach: a link out of the workspace, a loop, no permission.
		return unreadable, false, nil
	}
	var content io.Reader
	var mode string
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := root.Readlink(path)
		if err != nil {
			return unreadable, false, nil
		}
		content, mode = strings.NewReader(link), "120000"
	case info.Mode().IsRegular():
		file, err := root.Open(path)
		if err != nil {
			return unreadable, false, nil
		}
		defer file.Close()
		content, mode = file, "100644"
		if info.Mode()&0o111 != 0 {
			mode = "100755"
		}
	case info.IsDir():
		return dirEntry, false, nil
	default:
		return unreadable, false, nil
	}
	sha, err := gitInput(ctx, r.Dir, nil, content, "hash-object", "--stdin")
	if err != nil {
		return treeEntry{}, false, err
	}
	return treeEntry{mode, strings.TrimSpace(sha)}, true, nil
}

// entryOrDir is entry, with a directory in the tree read as dirEntry.
func (r *Repo) entryOrDir(ctx context.Context, commit, path string) (treeEntry, error) {
	entry, err := r.entry(ctx, commit, path)
	if errors.Is(err, errNotAFile) {
		return dirEntry, nil
	}
	return entry, err
}

func (r *Repo) writePath(ctx context.Context, dir, commit, path string) error {
	entry, err := r.entry(ctx, commit, path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if entry.sha == "" {
		err := root.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	body, err := r.git(ctx, nil, "cat-file", "blob", entry.sha)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write beside the destination and rename, so an interrupted recovery
	// leaves either the old or the merged content for the next WAL pass.
	tmp := path + ".steve-" + rand.Text()
	defer root.Remove(tmp)
	if entry.mode == "120000" {
		if err := root.Symlink(body, tmp); err != nil {
			return err
		}
	} else {
		mode := os.FileMode(0o644)
		if entry.mode == "100755" {
			mode = 0o755
		}
		file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = file.WriteString(body)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return root.Rename(tmp, path)
}
