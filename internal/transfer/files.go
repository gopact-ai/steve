package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxFile = 64 << 20
const maxTree = 512 << 20

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=steve", "GIT_AUTHOR_EMAIL=steve@localhost", "GIT_COMMITTER_NAME=steve", "GIT_COMMITTER_EMAIL=steve@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return out, nil
}
func copyTree(source, dest string) error {
	var err error
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	if err := transferDirectory(dest); err != nil {
		return err
	}
	writeDir, err := os.MkdirTemp(filepath.Dir(dest), ".steve-transfer-write-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(writeDir)
	var total int64
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return transferDirectory(dest)
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(dest, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return transferDirectory(to)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if old, err := os.Readlink(to); err == nil {
				if old == target {
					return nil
				}
				return fmt.Errorf("target symlink changed: %s", rel)
			}
			return os.Symlink(target, to)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported workspace entry %s", rel)
		}
		total += info.Size()
		if info.Size() > maxFile || total > maxTree {
			return fmt.Errorf("workspace content limit exceeded at %s (64 MiB/file, 512 MiB/project); ignored files are included", rel)
		}
		if targetInfo, err := os.Lstat(to); err == nil && !targetInfo.Mode().IsRegular() {
			return fmt.Errorf("target file type changed: %s", rel)
		}
		if old, err := os.ReadFile(to); err == nil {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if digest(old) == digest(data) {
				return nil
			}
			return fmt.Errorf("target file differs from migration payload: %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.CreateTemp(writeDir, "file-")
		if err != nil {
			return err
		}
		defer os.Remove(out.Name())
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Chmod(info.Mode().Perm())
		}
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		// Publish only complete contents, without replacing a file created by
		// someone else during installation. A killed copy leaves no partial
		// destination that would make the exact import impossible to retry.
		return os.Link(out.Name(), to)
	})
}

func transferDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("migration destination is not a real directory: %s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(path, 0700)
}

// canonicalTransferPath resolves existing ancestors while retaining a not-yet
// created tail. Directory ownership must compare physical paths, not aliases.
func canonicalTransferPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		if _, err := os.Lstat(abs); err == nil {
			resolved, err := filepath.EvalSymlinks(abs)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no existing directory ancestor for %s", path)
		}
		tail = append(tail, filepath.Base(abs))
		abs = parent
	}
}
func snapshot(ctx context.Context, source string) ([]byte, string, []byte, error) {
	temp, err := os.MkdirTemp("", "steve-transfer-snapshot-")
	if err != nil {
		return nil, "", nil, err
	}
	defer os.RemoveAll(temp)
	tree := filepath.Join(temp, "tree")
	if err := copyTree(source, tree); err != nil {
		return nil, "", nil, err
	}
	if _, err := git(ctx, tree, "init", "--quiet"); err != nil {
		return nil, "", nil, err
	}
	if err := rawSnapshotAttributes(tree); err != nil {
		return nil, "", nil, err
	}
	if _, err := git(ctx, tree, "add", "--force", "-A", "--", "."); err != nil {
		return nil, "", nil, err
	}
	if _, err := git(ctx, tree, "commit", "--quiet", "--allow-empty", "-m", "offline project transfer snapshot"); err != nil {
		return nil, "", nil, err
	}
	head, err := git(ctx, tree, "rev-parse", "HEAD")
	if err != nil {
		return nil, "", nil, err
	}
	name := filepath.Join(temp, "snapshot.bundle")
	if _, err := git(ctx, tree, "bundle", "create", name, "--all"); err != nil {
		return nil, "", nil, err
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, "", nil, err
	}
	var history []byte
	if top, err := git(ctx, source, "rev-parse", "--show-toplevel"); err == nil {
		abs, _ := filepath.EvalSymlinks(source)
		if strings.TrimSpace(string(top)) == abs {
			historyFile := filepath.Join(temp, "history.bundle")
			if _, err := git(ctx, source, "bundle", "create", historyFile, "--all", "HEAD"); err != nil {
				return nil, "", nil, err
			}
			history, err = os.ReadFile(historyFile)
			if err != nil {
				return nil, "", nil, err
			}
		}
	}
	return data, strings.TrimSpace(string(head)), history, nil
}
func restoreSnapshot(ctx context.Context, data []byte, head, dest string) error {
	if len(head) != 40 && len(head) != 64 {
		return fmt.Errorf("invalid snapshot commit")
	}
	if _, err := hex.DecodeString(head); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "steve-transfer-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	bundle := filepath.Join(temp, "snapshot.bundle")
	if err := os.WriteFile(bundle, data, 0600); err != nil {
		return err
	}
	tree := filepath.Join(temp, "tree")
	if _, err := git(ctx, temp, "clone", "--quiet", "--no-checkout", bundle, tree); err != nil {
		return err
	}
	if err := rawSnapshotAttributes(tree); err != nil {
		return err
	}
	if _, err := git(ctx, tree, "checkout", "--quiet", head); err != nil {
		return err
	}
	return copyTree(tree, dest)
}

func rawSnapshotAttributes(tree string) error {
	// info/attributes overrides every tracked .gitattributes file. A migration
	// snapshot must preserve raw bytes, even when the project asks normal Git
	// add/checkout to convert newlines, encoding, ident or custom filters.
	return os.WriteFile(filepath.Join(tree, ".git", "info", "attributes"), []byte("* -text -ident -filter -working-tree-encoding\n"), 0600)
}

func restoreGitHistory(ctx context.Context, history []byte, commit, branch, home, stage string) error {
	if len(history) == 0 {
		return nil
	}
	if len(commit) != 40 && len(commit) != 64 {
		return fmt.Errorf("invalid source git head")
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return err
	}
	if branch != "" && !strings.HasPrefix(branch, "refs/heads/") {
		return fmt.Errorf("invalid source git branch")
	}
	// rev-parse alone can discover an ancestor repository while restoring a
	// nested one. Only this directory's own repository is an import replay.
	if _, statErr := os.Lstat(filepath.Join(home, ".git")); statErr == nil {
		current, err := git(ctx, home, "rev-parse", "HEAD")
		if err == nil {
			if strings.TrimSpace(string(current)) == commit {
				// A previous import may have stopped after publishing HEAD but
				// before building the index. Mixed reset leaves raw files intact.
				_, err := git(ctx, home, "reset", "--mixed", commit)
				return err
			}
			return fmt.Errorf("target git history changed during interrupted import")
		}
	}
	if _, err := git(ctx, home, "init", "--quiet"); err != nil {
		return err
	}
	path := filepath.Join(stage, "source-history.bundle")
	if err := os.WriteFile(path, history, 0600); err != nil {
		return err
	}
	if _, err := git(ctx, home, "fetch", "--quiet", "--update-head-ok", path, "refs/*:refs/*"); err != nil {
		return err
	}
	if branch != "" {
		if _, err := git(ctx, home, "symbolic-ref", "HEAD", branch); err != nil {
			return err
		}
	} else {
		if _, err := git(ctx, home, "update-ref", "--no-deref", "HEAD", commit); err != nil {
			return err
		}
	}
	_, err := git(ctx, home, "reset", "--mixed", commit)
	return err
}
