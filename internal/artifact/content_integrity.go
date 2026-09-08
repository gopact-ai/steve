package artifact

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gopact-ai/steve/internal/contentreplica"
)

const maxContentObjects = 1 << 20

type ContentLimits struct {
	MaxObjects       int
	MaxExpandedBytes int64
}

func (l ContentLimits) effective() (ContentLimits, error) {
	if l.MaxObjects == 0 {
		l.MaxObjects = maxContentObjects
	}
	if l.MaxExpandedBytes == 0 {
		l.MaxExpandedBytes = 2 << 30
	}
	if l.MaxObjects < 1 || l.MaxObjects > maxContentObjects || l.MaxExpandedBytes < 1 {
		return l, contentreplica.ErrInvalid
	}
	return l, nil
}

// verifyContentCommit verifies every object in the selected commit's closure,
// including blob hashes. cat-file -e alone only proves the commit object exists;
// fsck over a whole cache would also fail on unrelated, unreferenced damage.
func (s *Store) verifyContentCommit(ctx context.Context, repo *Repo, commit string) ([]string, error) {
	return s.verifyContentCommitWith(ctx, repo, commit, map[string]int64{})
}

func (s *Store) verifyContentCommitWith(ctx context.Context, repo *Repo, commit string, verified map[string]int64) ([]string, error) {
	if !shaPattern.MatchString(commit) {
		return nil, contentreplica.ErrIntegrity
	}
	limits, err := s.ContentLimits.effective()
	if err != nil {
		return nil, err
	}
	raw, truncated, err := runBounded(ctx, repo.Dir, limits.MaxObjects*65, "--no-replace-objects", "rev-list", "--objects", "--no-object-names", commit)
	if err != nil {
		return nil, fmt.Errorf("%w: incomplete Git closure: %w", contentreplica.ErrIntegrity, err)
	}
	if truncated {
		return nil, contentreplica.ErrTooLarge
	}
	ids := strings.Fields(string(raw))
	if len(ids) == 0 || len(ids) > limits.MaxObjects {
		return nil, contentreplica.ErrTooLarge
	}
	remaining := limits.MaxExpandedBytes
	var pending []string
	for _, id := range ids {
		if size, ok := verified[id]; ok {
			if size > remaining {
				return nil, contentreplica.ErrTooLarge
			}
			remaining -= size
		} else {
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		return ids, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects", "cat-file", "--batch")
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo.Dir, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = strings.NewReader(strings.Join(pending, "\n") + "\n")
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waited := false
	defer func() {
		if !waited {
			// Abandoning a failed verification: the process is cancelled and
			// collected; its exit status adds nothing to the integrity error.
			cancel()
			_ = output.Close()
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReader(output)
	for _, id := range pending {
		if !shaPattern.MatchString(id) || len(id) != len(commit) {
			return nil, contentreplica.ErrIntegrity
		}
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, contentreplica.ErrIntegrity
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != id {
			return nil, contentreplica.ErrIntegrity
		}
		switch fields[1] {
		case "commit", "tree", "blob", "tag":
		default:
			return nil, contentreplica.ErrIntegrity
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, contentreplica.ErrIntegrity
		}
		if size > remaining {
			return nil, contentreplica.ErrTooLarge
		}
		remaining -= size
		var digest hash.Hash
		if len(id) == 40 {
			digest = sha1.New()
		} else {
			digest = sha256.New()
		}
		// A hash never fails to write.
		_, _ = fmt.Fprintf(digest, "%s %d%c", fields[1], size, byte(0))
		if _, err := io.CopyN(digest, reader, size); err != nil {
			return nil, contentreplica.ErrIntegrity
		}
		end, err := reader.ReadByte()
		if err != nil || end != '\n' || hex.EncodeToString(digest.Sum(nil)) != id {
			return nil, fmt.Errorf("%w: Git object %s content differs", contentreplica.ErrIntegrity, id)
		}
		verified[id] = size
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return nil, contentreplica.ErrIntegrity
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		return nil, contentreplica.ErrIntegrity
	}
	return ids, nil
}

// expandContentPacks removes packs from the private staging object's search
// path before unpacking them. Unpacking beside an existing pack can otherwise
// skip the very objects being repaired. Target repository data is untouched.
func expandContentPacks(ctx context.Context, repo *Repo) error {
	packDir := filepath.Join(repo.Dir, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	inputDir := filepath.Join(repo.Dir, "content-pack-input")
	if err := os.Rename(packDir, inputDir); err != nil {
		return err
	}
	if err := os.Mkdir(packDir, 0700); err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".pack") {
			continue
		}
		if !entry.Type().IsRegular() {
			return contentreplica.ErrIntegrity
		}
		file, err := os.Open(filepath.Join(inputDir, entry.Name()))
		if err != nil {
			return err
		}
		_, err = gitInput(ctx, repo.Dir, []string{"GIT_OBJECT_DIRECTORY=" + filepath.Join(repo.Dir, "objects"), "GIT_ALTERNATE_OBJECT_DIRECTORIES="}, file, "unpack-objects")
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Install exact verified IDs as loose objects. This avoids both fetch
// negotiation skipping a known commit and an old corrupt pack being preferred
// over a newly imported pack. Unrelated objects and worktree metadata remain.
func installVerifiedObjects(source, target *Repo, ids []string) error {
	src, err := os.OpenRoot(filepath.Join(source.Dir, "objects"))
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(filepath.Join(target.Dir, "objects"))
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, id := range ids {
		name := id[:2] + "/" + id[2:]
		info, err := src.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return contentreplica.ErrIntegrity
		}
		if err := dst.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		input, err := src.Open(name)
		if err != nil {
			return err
		}
		temp := ".content-repair-" + rand.Text()
		output, err := dst.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		input.Close()
		syncErr := output.Sync()
		closeErr := output.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			dst.Remove(temp)
			return err
		}
		if err := dst.Rename(temp, name); err != nil {
			dst.Remove(temp)
			return err
		}
		if err := syncObjectDir(dst, filepath.Dir(name)); err != nil {
			return err
		}
	}
	return syncObjectDir(dst, ".")
}

// A corrupt old pack can be chosen before a newly written loose object.
// Retain it outside Git's lookup path after verified replacements are on disk;
// never delete its unrelated, potentially useful objects during recovery.
func quarantineInvalidContentPacks(ctx context.Context, repo *Repo) error {
	root, err := os.OpenRoot(filepath.Join(repo.Dir, "objects"))
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(filepath.Join(repo.Dir, "objects", "pack"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".idx") {
			continue
		}
		if !entry.Type().IsRegular() {
			return contentreplica.ErrIntegrity
		}
		cmd := exec.CommandContext(ctx, "git", "verify-pack", filepath.Join(repo.Dir, "objects", "pack", entry.Name()))
		cmd.Env = append(os.Environ(), "GIT_DIR="+repo.Dir, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		cmd.Stdout = io.Discard
		cmd.Stderr = &bundleWriter{into: io.Discard, limit: 64 << 10}
		err := cmd.Run()
		if err == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 1 {
			return fmt.Errorf("Git pack verification could not complete: %w", err)
		}
		base := strings.TrimSuffix(entry.Name(), ".idx")
		destination := "content-quarantine/" + rand.Text()
		if err := root.MkdirAll(destination, 0700); err != nil {
			return err
		}
		for _, suffix := range []string{".pack", ".idx", ".rev", ".bitmap", ".keep", ".promisor"} {
			name := "pack/" + base + suffix
			info, err := root.Lstat(name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return contentreplica.ErrIntegrity
			}
			if err := root.Rename(name, destination+"/"+base+suffix); err != nil {
				return err
			}
		}
		if err := syncObjectDir(root, destination); err != nil {
			return err
		}
		if err := syncObjectDir(root, "content-quarantine"); err != nil {
			return err
		}
		if err := syncObjectDir(root, "pack"); err != nil {
			return err
		}
	}
	return syncObjectDir(root, ".")
}

func syncObjectDir(root *os.Root, name string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
