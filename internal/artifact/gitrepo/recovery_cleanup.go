package gitrepo

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/ops"
)

// RecoveryContainer inspects only the platform's fixed container. A recorded
// directory identity permits continuing its own partial deletion, never deleting
// a replacement directory after its marker disappeared.
func RecoveryContainer(ctx context.Context, root, container, recovery, base, expected, rootIdentity string, remove bool) (ops.Result, error) {
	var result ops.Result
	identityBytes, decodeErr := hex.DecodeString(strings.TrimPrefix(recovery, "workspace-recovery-"))
	if !ValidSHA(base) || decodeErr != nil || len(identityBytes) != 16 || recovery != "workspace-recovery-"+hex.EncodeToString(identityBytes) {
		return result, ErrPreparedWorkspaceChanged
	}
	name := "wt-" + base[:12] + "-" + recovery
	if filepath.Clean(container) != filepath.Join(filepath.Clean(root), "worktrees", name) {
		return result, ErrPreparedWorkspaceChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	installation, err := os.OpenRoot(root)
	if err != nil {
		return result, err
	}
	defer installation.Close()
	rootFile, err := installation.Open(".")
	if err != nil {
		return result, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return result, err
	}
	result.RootIdentity, err = recoveryDirectoryHandleIdentity(rootFile, rootInfo)
	if err != nil {
		return result, err
	}
	if rootIdentity != "" && result.RootIdentity != rootIdentity {
		return result, ErrPreparedWorkspaceChanged
	}
	parent, err := os.OpenRoot(filepath.Join(root, "worktrees"))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer parent.Close()
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, ErrPreparedWorkspaceChanged
	}
	openedRoot, err := parent.OpenRoot(name)
	if err != nil {
		return result, err
	}
	defer openedRoot.Close()
	openedFile, err := openedRoot.Open(".")
	if err != nil {
		return result, err
	}
	defer openedFile.Close()
	openedInfo, err := openedFile.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return result, ErrPreparedWorkspaceChanged
	}
	identity, err := recoveryDirectoryHandleIdentity(openedFile, openedInfo)
	if err != nil {
		return result, err
	}
	result.Has, result.Identity = true, identity
	if expected != "" && expected != identity {
		return result, ErrPreparedWorkspaceChanged
	}
	marker := filepath.Join(name, recoveryPreparationMarker)
	mark, err := parent.Lstat(marker)
	if os.IsNotExist(err) {
		if expected == "" {
			return result, ErrPreparedWorkspaceChanged
		}
	} else {
		if err != nil {
			return result, err
		}
		if !mark.Mode().IsRegular() || mark.Size() != int64(len(recovery)+len(base)+2) {
			return result, ErrPreparedWorkspaceChanged
		}
		raw, err := parent.ReadFile(marker)
		if err != nil || string(raw) != recovery+"\n"+base+"\n" {
			return result, ErrPreparedWorkspaceChanged
		}
	}
	if err := recoveryContainerEntries(openedRoot); err != nil {
		return result, err
	}
	if !remove {
		return result, nil
	}
	if expected == "" {
		return result, errors.New("recovery removal requires recorded container identity")
	}
	return removeRecoveryContainer(ctx, parent, name, info, result)
}

func removeRecoveryContainer(ctx context.Context, parent *os.Root, name string, info os.FileInfo, result ops.Result) (ops.Result, error) {
	owned, err := parent.OpenRoot(name)
	if err != nil {
		return result, err
	}
	defer owned.Close()
	dir, err := owned.Open(".")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrPreparedWorkspaceChanged
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if entry.Name() == recoveryPreparationMarker {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := owned.RemoveAll(entry.Name()); err != nil {
			return result, err
		}
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		return result, ErrPreparedWorkspaceChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := owned.Remove(recoveryPreparationMarker); err != nil && !os.IsNotExist(err) {
		return result, err
	}
	// Remove the exact empty basename only; never recurse through this path.
	// A non-cooperative external replacement between this check and unlink is
	// not a whole-directory transaction and remains outside declaration fencing.
	current, err = parent.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		return result, ErrPreparedWorkspaceChanged
	}
	if err := parent.Remove(name); err != nil {
		return result, err
	}
	if _, err := parent.Lstat(name); !os.IsNotExist(err) {
		if err != nil {
			return result, err
		}
		return result, ErrPreparedWorkspaceChanged
	}
	result.Has = false
	return result, nil
}

// VerifyRecoveryContent also checks ignored/untracked physical entities. A
// matching tracked snapshot alone cannot justify deleting excluded user bytes.
func (r *Repo) VerifyRecoveryContent(ctx context.Context, commit, work string) error {
	if err := r.VerifyCheckout(ctx, commit, work); err != nil {
		return err
	}
	out, err := r.Git(ctx, []string{"GIT_WORK_TREE=" + work}, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if out != "" {
		return errors.New("recovery copy has ignored content that is not preserved in its frozen artifact")
	}
	return nil
}

// VerifyRecoveryRemainder validates every surviving physical entity before a
// recorded partial removal continues. Missing files may be this removal's own;
// added/changed/ignored/nested entities are never inferred preserved from absence.
func (r *Repo) VerifyRecoveryRemainder(ctx context.Context, commit, work string) error {
	if _, err := os.Lstat(work); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(work, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(work, full)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		state, err := r.pathState(ctx, work, commit, commit, filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		if state != "merged" {
			return ErrPreparedWorkspaceChanged
		}
		return nil
	})
}

func recoveryContainerEntries(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "work" && entry.Name() != recoveryPreparationMarker {
			return ErrPreparedWorkspaceChanged
		}
	}
	return nil
}
