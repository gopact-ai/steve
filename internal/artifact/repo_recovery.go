package artifact

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type treeEntry struct{ mode, sha string }

func (r *Repo) entry(ctx context.Context, commit, path string) (treeEntry, error) {
	out, err := r.git(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, "ls-tree", "--full-tree", "-z", commit, "--", path)
	if err != nil || out == "" {
		return treeEntry{}, err
	}
	meta, _, ok := strings.Cut(out, "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 || fields[1] != "blob" {
		return treeEntry{}, fmt.Errorf("artifact: %s is not a file in %s", path, commit)
	}
	return treeEntry{fields[0], fields[2]}, nil
}

func (r *Repo) pathState(ctx context.Context, dir, from, to, path string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var current treeEntry
	info, err := root.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		var sha string
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := root.Readlink(path)
			if err != nil {
				return "", err
			}
			sha, err = gitInput(ctx, r.Dir, nil, strings.NewReader(link), "hash-object", "--stdin")
			if err != nil {
				return "", err
			}
			current.mode = "120000"
		case info.Mode().IsRegular():
			file, err := root.Open(path)
			if err != nil {
				return "", err
			}
			sha, err = gitInput(ctx, r.Dir, nil, file, "hash-object", "--stdin")
			file.Close()
			if err != nil {
				return "", err
			}
			current.mode = "100644"
			if info.Mode()&0o111 != 0 {
				current.mode = "100755"
			}
		default:
			return "other", nil
		}
		current.sha = strings.TrimSpace(sha)
	}
	merged, err := r.entry(ctx, to, path)
	if err != nil {
		return "", err
	}
	old, err := r.entry(ctx, from, path)
	if err != nil {
		return "", err
	}
	if current == merged {
		return "merged", nil
	}
	if current == old {
		return "old", nil
	}
	return "other", nil
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
